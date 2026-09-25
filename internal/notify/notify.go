// Package notify carries a filed gate to a person.
//
// The mill does not notify anyone itself, and this package does not either:
// skift's notifier is what wakes a human, and until the thicket bridge
// consumes skift's events the deployment is handed the gate and does
// whatever it wants with it (docs/prd.md D1, docs/architecture.md §"Gate
// steps"). The seam is one **configured command**: the mill writes the gate
// to its standard input as JSON and reads its exit status. What that command
// does — `thicket send`, a webhook, a mail — is the deployment's decision and
// not the mill's, and the shipped example under `contrib/notify/` is an
// example rather than an implementation this package knows about.
//
// Two rules make it safe to be a stopgap:
//
//   - Notification is not the run's work. A command that exits non-zero,
//     hangs past its bound, or does not exist at all is recorded and nothing
//     more; the gate is filed either way and the run waits for its answer.
//   - The command is spawned through the workspace's chokepoint, like every
//     other process the mill starts (docs/core-principles.md §3).
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/steading-ai/mill/internal/workspace"
)

// Timeout bounds one notification. A notifier that has not answered in half
// a minute is not going to: the gate is already filed, and holding the run's
// lease open on a command nobody is watching buys nothing.
const Timeout = 30 * time.Second

// OutputLimit is how much of the command's standard error is kept for the
// record. What is being stored is a diagnostic, not a transcript.
const OutputLimit = 8 << 10

// killSignal ends a command that outlasted its bound. It is the hard one:
// the process was given the whole window to say something and did not, and
// the run is waiting on this before it can give its lease back.
const killSignal = syscall.SIGKILL

// Kind is what a carried gate decides, which is the one field a notifier
// routes on.
type Kind string

const (
	// KindPlan is a gate about the work itself: a plan to approve, or
	// anything else a workflow stops on that authorizes no act of its own.
	KindPlan Kind = "plan"
	// KindMerge is a merge gate, whose approval merges the branch.
	KindMerge Kind = "merge"
	// KindPark is the circuit breaker asking what to do with a run it
	// stopped.
	KindPark Kind = "park"
	// KindProbe is `mill doctor` asking whether the command works. A
	// notifier that treats it as a real gate wakes somebody for nothing, so
	// it carries the kind and no gate at all.
	KindProbe Kind = "probe"
)

// Notice is one filed gate, as the command receives it. The JSON field names
// are the wire: a notify command reads them, and changing one changes every
// deployment's script.
type Notice struct {
	// Kind is what the gate decides.
	Kind Kind `json:"kind"`
	// Gate is the gate node's reference, which is where the answer is
	// written.
	Gate string `json:"gate,omitempty"`
	// Node is the node the gate blocks.
	Node string `json:"node,omitempty"`
	// Run is the run that filed it.
	Run string `json:"run,omitempty"`
	// Title and Body are the ask, already rendered.
	Title string `json:"title,omitempty"`
	// Body is the ask's body.
	Body string `json:"body,omitempty"`
}

// Result is how one notification ended: the facts the ledger records.
type Result struct {
	// Status is the command's exit status, or -1 when a signal ended it —
	// which is what the timeout does — or when it never ran at all.
	Status int32
	// Detail is what the command said for itself: its standard error, or the
	// reason it could not be run.
	Detail string
}

// OK reports whether the command accepted the notice.
func (r Result) OK() bool { return r.Status == 0 }

// String renders a result for a log line or a probe's reason.
func (r Result) String() string {
	if r.Detail == "" {
		return fmt.Sprintf("exit %d", r.Status)
	}
	return fmt.Sprintf("exit %d: %s", r.Status, firstLine(r.Detail))
}

// Notifier carries a notice to whoever the deployment says. It returns an
// error only when there was nothing to run; a command that ran and failed is
// a [Result] with a non-zero status, because that is a fact about the world
// and not about the mill.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) (Result, error)
}

// Command is a notifier that runs a configured command with the notice as
// JSON on its standard input.
type Command struct {
	// Path is the program to run.
	Path string
	// Args are the arguments before the notice, if the configuration named
	// any.
	Args []string
	// Timeout bounds one run. Zero takes [Timeout].
	Timeout time.Duration
	// Exec is the chokepoint the process is spawned through. Nil spawns as
	// the mill's own account, which is what a notifier is: the deployment's
	// own command, not a session's.
	Exec workspace.Execer
}

// Parse reads a configured command: the program and its arguments, split on
// whitespace. There is no shell — a deployment that wants pipes and
// variables writes a script and names it, which is what
// `contrib/notify/thicket-send.sh` is. An empty string is no notifier at
// all, which is a deployment that has not configured one rather than an
// error.
func Parse(command string) *Command {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil
	}
	return &Command{Path: fields[0], Args: fields[1:]}
}

// Notify runs the command with the notice on its standard input.
func (c *Command) Notify(ctx context.Context, notice Notice) (Result, error) {
	if c == nil || c.Path == "" {
		return Result{}, errors.New("notify: no command is configured")
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		return Result{}, fmt.Errorf("notify: encode the notice: %w", err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	// The bound is the notifier's own, not the run's: a run whose step
	// context is already cancelled still files its gate and still says so.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	spawner := c.Exec
	if spawner == nil {
		spawner = workspace.Spawner{}
	}
	process, err := spawner.Exec(ctx, workspace.Command{Path: c.Path, Args: c.Args})
	if err != nil {
		return Result{Status: -1, Detail: err.Error()}, nil
	}
	return wait(ctx, process, append(payload, '\n')), nil
}

// wait feeds the notice in, reads the diagnostics out, and reports how the
// command ended. A command still running when the bound elapses is signalled
// through its process group, so nothing it spawned outlives it.
func wait(ctx context.Context, process *workspace.Process, payload []byte) Result {
	done := make(chan Result, 1)
	go func() {
		_, writeErr := process.Stdin.Write(payload)
		closeErr := process.Stdin.Close()
		// Standard output is the command's business; standard error is the
		// diagnostic the ledger keeps.
		_, _ = io.Copy(io.Discard, process.Stdout)
		stderr, _ := io.ReadAll(io.LimitReader(process.Stderr, OutputLimit))
		result := process.Wait()
		detail := strings.TrimSpace(string(stderr))
		if detail == "" && result.Status != 0 {
			detail = writeDetail(writeErr, closeErr)
		}
		done <- Result{Status: result.Status, Detail: detail}
	}()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		_ = process.Signal(killSignal)
		select {
		case result := <-done:
			result.Detail = strings.TrimSpace("the notify command did not answer in time; it was killed. " + result.Detail)
			result.Status = -1
			return result
		case <-time.After(time.Second):
			return Result{Status: -1, Detail: "the notify command did not answer in time and did not die"}
		}
	}
}

// writeDetail is what to say about a command that failed and wrote no
// diagnostic of its own. A notifier that closed its input before reading the
// notice is the interesting case: the exit status alone does not say so.
func writeDetail(writeErr, closeErr error) string {
	switch {
	case writeErr != nil:
		return "the notice could not be written to the command: " + writeErr.Error()
	case closeErr != nil:
		return "the command's standard input could not be closed: " + closeErr.Error()
	default:
		return ""
	}
}

// firstLine is a diagnostic trimmed to one line.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
