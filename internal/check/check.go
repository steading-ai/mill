// Package check runs a check step: a deterministic command, through the
// workspace, read against a contract.
//
// A check is what makes a workflow enforced rather than described. A skill
// can recommend a script; only a check step makes the mill run it and act on
// the result (docs/domain-model.md §Check). There is no session here and no
// model: the command's exit code, its stdout in the shape the definition
// names, and its stderr are the whole of what a check produces, and the exit
// code is the evidence.
//
// Three things follow from that.
//
// The command is spawned through [github.com/steading-ai/mill/internal/workspace]'s
// Exec and nowhere else, so cwd, uid and environment are the workspace's to
// set (docs/core-principles.md §3). This package never touches os/exec, and
// lint enforces it.
//
// The command path is resolved against the skill directory, then the
// workflow's, and must stay inside whichever it resolved in. A check's
// command is part of a versioned definition; a path that climbs out of it is
// a definition reaching for something the version does not carry.
//
// Stderr is diagnostics and stdout is data — the agent-skills script
// convention, which the `stdout` contract is written in. A failing check
// whose `feedback` is `revise` hands its stderr back to the session verbatim
// as the next prompt, because the diagnostic the script wrote is better than
// anything the mill could say about it.
package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/evidence"
	"github.com/steading-ai/mill/internal/workspace"
)

// OutputLimit is how much of a stream is kept. A check that writes more than
// this has stopped being a diagnostic, and the mill holds both streams in
// memory to put them in the ledger and in a prompt.
const OutputLimit = 1 << 20

// Request is one check step to run.
type Request struct {
	// Name is the step's name, for the record of what produced the result.
	Name string
	// Check is the step's contract: the command, the exit codes that pass,
	// the shape of stdout, and what a failure does with stderr.
	Check *millv1.Check
	// SkillDir is the directory of the step's skill, when it has one. A
	// relative command is resolved here first, which is what lets a skill
	// carry the script its check runs.
	SkillDir string
	// WorkflowDir is the directory the workflow was loaded from. A relative
	// command that the skill does not carry is resolved here.
	WorkflowDir string
	// Sink receives the check's own activity records — its resolved command
	// and contract, then its output as it streams — as Run produces them.
	// Nil keeps Run exactly as it was before Sink existed.
	Sink Sink
}

// Sink is where a check's own activity records come from: its resolved
// command and contract before it runs, and its output as the process
// produces it (docs/activity.md's check_started and check_output). A check
// has no session and no events, so nothing else reports its progress before
// Run returns.
type Sink interface {
	// Started is called once, with the resolved command and the contract it
	// will be judged against, before the process spawns.
	Started(command []string, contract *millv1.Check)
	// Output is called for every read from stream ("stdout" or "stderr") as
	// Run drains it, not batched until Run returns.
	Output(stream string, data []byte)
}

// Result is what running a check produced.
type Result struct {
	// Exit is how the process ended, in the shape `exit` evidence carries.
	Exit *harnessv1.ProcessExit
	// Passed reports whether the exit status was one the definition names
	// *and* stdout met its contract.
	Passed bool
	// Stdout is what the command wrote, kept when the contract is `text` or
	// `json` and dropped when it is `none`.
	Stdout string
	// JSON is stdout parsed and validated, when the contract is `json`.
	JSON *structpb.Struct
	// Stderr is the command's diagnostics, always kept.
	Stderr string
	// Reason says why a check that ran did not pass. Empty when it passed.
	Reason string
}

// Run executes the check and reads its result against the contract. The
// error return is the mill failing to run the check at all — a command that
// does not resolve, a workspace that cannot spawn. A command that ran and
// failed is a [Result] with Passed false, because that is a fact about the
// work and not about the mill.
func Run(ctx context.Context, execer workspace.Execer, request Request) (Result, error) {
	command, err := Command(request)
	if err != nil {
		return Result{}, err
	}
	if request.Sink != nil {
		request.Sink.Started(append([]string{command.Path}, command.Args...), request.Check)
	}
	process, err := execer.Exec(ctx, command)
	if err != nil {
		return Result{}, fmt.Errorf("check %s: %w", request.Name, err)
	}
	// The step's budget is the context's: when it ends, the process group
	// is killed so the streams close and the wait below returns, and the
	// exit is recorded as the budget's below. A workspace whose exec is
	// stopped another way — the machine's, by its unit — is stopped that
	// way too; the signal reaches whatever this side can still see.
	stop := context.AfterFunc(ctx, func() { _ = process.Signal(syscall.SIGKILL) })
	defer stop()

	// Both streams are drained concurrently: a command that fills the pipe
	// mill is not reading blocks forever, and a check that hangs is worse
	// than one that fails.
	var wait sync.WaitGroup
	var stdout, stderr string
	wait.Add(2)
	go func() { defer wait.Done(); stdout = drain(tee(process.Stdout, "stdout", request.Sink)) }()
	go func() { defer wait.Done(); stderr = drain(tee(process.Stderr, "stderr", request.Sink)) }()
	if err := process.Stdin.Close(); err != nil {
		return Result{}, fmt.Errorf("check %s: close stdin: %w", request.Name, err)
	}
	wait.Wait()
	exit := process.Wait()

	result := Result{
		Exit:   &harnessv1.ProcessExit{Status: exit.Status, Signal: exit.Signal},
		Stderr: stderr,
	}
	if exit.Signal != 0 {
		result.Exit.Status = -1
		result.Exit.Reason = harnessv1.ExitReason_EXIT_REASON_CANCELLED
	}
	if err := ctx.Err(); err != nil && result.Exit.GetStatus() < 0 {
		result.Exit.Reason = harnessv1.ExitReason_EXIT_REASON_TIMEOUT
	}
	result.readStdout(request.Check, stdout)
	result.readExit(request.Check)
	return result, nil
}

// readExit decides whether the status the process ended with is one the
// definition names. A stdout contract that stdout did not meet has already
// failed the check, and the exit code does not override it: a command that
// exits 0 with output the schema refuses did not do what it said it did.
func (r *Result) readExit(check *millv1.Check) {
	if r.Reason != "" {
		return
	}
	if r.Exit.GetStatus() < 0 {
		r.Reason = "the command never ran"
		return
	}
	for _, code := range evidence.Codes(check) {
		if r.Exit.GetStatus() == code {
			r.Passed = true
			return
		}
	}
	r.Reason = fmt.Sprintf("exited %d", r.Exit.GetStatus())
}

// readStdout reads stdout in the shape the contract names. `none` ignores
// it, `text` keeps it, and `json` parses and validates it — and a document
// that does not validate fails the check, naming the first violating path,
// because a check's stdout is a contract and not a courtesy.
func (r *Result) readStdout(check *millv1.Check, stdout string) {
	switch check.GetStdout() {
	case millv1.StdoutShape_STDOUT_SHAPE_JSON:
		r.Stdout = stdout
		document := map[string]any{}
		if err := json.Unmarshal([]byte(stdout), &document); err != nil {
			r.Reason = fmt.Sprintf("stdout is not a JSON object: %s", err)
			return
		}
		if schema := check.GetStdoutSchema(); strings.TrimSpace(schema) != "" {
			if err := evidence.ValidateJSON(schema, document); err != nil {
				r.Reason = "stdout does not validate: " + err.Error()
				return
			}
		}
		value, err := structpb.NewStruct(document)
		if err != nil {
			r.Reason = fmt.Sprintf("stdout is not a JSON document: %s", err)
			return
		}
		r.JSON = value
	case millv1.StdoutShape_STDOUT_SHAPE_TEXT:
		r.Stdout = stdout
	case millv1.StdoutShape_STDOUT_SHAPE_NONE, millv1.StdoutShape_STDOUT_SHAPE_UNSPECIFIED:
		// Ignored, and not kept: a contract that says stdout means nothing
		// is a contract that says do not read it.
	}
}

// Feedback is what a failed check hands back to the session as its next
// prompt: the command's own stderr, verbatim.
//
// It is empty when the check passed, when the definition's `feedback` is
// `park` — a failure the workflow does not want a session to have another go
// at — and when the command wrote nothing to stderr, which is a check with
// no diagnostic to give.
func Feedback(check *millv1.Check, result Result) string {
	if result.Passed || check.GetFeedback() != millv1.Feedback_FEEDBACK_REVISE {
		return ""
	}
	return strings.TrimRight(result.Stderr, "\n")
}

// ErrOutsideDefinition is a command path that leaves the directory it
// resolved in. A check runs what its own version carries and nothing else.
var ErrOutsideDefinition = errors.New("check: the command is outside the definition's directory")

// Command is the process a check step spawns: the command resolved, the
// arguments as the definition wrote them.
//
// A path with a separator in it is relative to the skill directory, then the
// workflow's, and must exist in one of them. A bare name is left alone for
// the workspace to resolve on PATH, which is how a check runs `make` or
// `git` without the definition carrying one.
func Command(request Request) (workspace.Command, error) {
	argv := request.Check.GetCommand()
	if len(argv) == 0 {
		return workspace.Command{}, fmt.Errorf("check %s: the definition names no command", request.Name)
	}
	path, err := resolve(request, argv[0])
	if err != nil {
		return workspace.Command{}, err
	}
	// The environment is the workspace's to set and no part of the check's
	// contract: a check names a command and reads its result
	// (docs/core-principles.md §3).
	return workspace.Command{Path: path, Args: argv[1:]}, nil
}

func resolve(request Request, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("check %s: %q is absolute: a command is named relative to the skill or the workflow: %w",
			request.Name, name, ErrOutsideDefinition)
	}
	if !strings.ContainsRune(name, filepath.Separator) {
		// A bare name is a program on PATH; the workspace resolves it.
		return name, nil
	}
	var tried []string
	for _, dir := range []string{request.SkillDir, request.WorkflowDir} {
		if dir == "" {
			continue
		}
		root, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("check %s: %w", request.Name, err)
		}
		candidate := filepath.Join(root, name)
		if !within(root, candidate) {
			return "", fmt.Errorf("check %s: %q leaves %s: %w", request.Name, name, dir, ErrOutsideDefinition)
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		tried = append(tried, candidate)
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("check %s: %q is relative and the step names no skill or workflow directory to resolve it in",
			request.Name, name)
	}
	return "", fmt.Errorf("check %s: %q is not in %s", request.Name, name, strings.Join(tried, " or "))
}

func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// tee wraps r so every read it produces also reaches sink's Output for
// stream, as Run drains it rather than once it has finished. A nil sink
// returns r unchanged.
func tee(r io.Reader, stream string, sink Sink) io.Reader {
	if sink == nil {
		return r
	}
	return io.TeeReader(r, sinkWriter{stream: stream, sink: sink})
}

// sinkWriter adapts a check's Sink to an io.Writer, so [tee] can hand it to
// io.TeeReader. It always succeeds: a dropped activity record is not a
// failure of the check reading its own output.
type sinkWriter struct {
	stream string
	sink   Sink
}

func (w sinkWriter) Write(p []byte) (int, error) {
	w.sink.Output(w.stream, p)
	return len(p), nil
}

// drain reads a stream to its end, keeping at most [OutputLimit] of it.
func drain(reader io.Reader) string {
	var b strings.Builder
	if _, err := io.Copy(&b, io.LimitReader(reader, OutputLimit)); err != nil {
		return b.String()
	}
	// Whatever is past the limit is still read, so the child is never
	// blocked on a pipe nobody is emptying.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return b.String()
	}
	return b.String()
}
