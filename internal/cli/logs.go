package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/encoding/protojson"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/workspace"
)

// `mill logs` reads a run's activity over the Logs RPC and renders it back
// into the lines an operator reads: the store holds the record now, not the
// worker's own host, so a client sees a run's log wherever it was made
// (docs/activity.md §The RPC). The redaction happened at write time, so
// this command has nothing to scrub: it decodes and renders.

// errLogsWaiting, errLogsParked and errLogsStreamBroken are what a followed
// stream's end becomes once ExitCode has translated it: docs/activity.md's
// exit table for `mill logs -f`. 2 is stead's doctor's code for "could not
// tell"; 75 is ExitFenced. None collide.
var (
	errLogsWaiting      = errors.New("waiting on a gate")
	errLogsParked       = errors.New("parked on a gate")
	errLogsStreamBroken = errors.New("stream broke")
)

type logsOptions struct {
	readOptions
	attempt int64
	json    bool
	tail    int64
	noTail  bool
	noPager bool
	end     bool
}

func newLogsCommand() *cobra.Command {
	opts := logsOptions{}
	cmd := &cobra.Command{
		Use:   "logs <run>",
		Short: "Read a run's activity, live or in full",
		Long: `Read a run's activity: every attempt of every step, oldest first, rendered
the way the session's events read as a log. With no --step this is the
whole run; --step narrows to one step, --attempt to one attempt of it.

With no --follow the whole answer is printed once, through $PAGER on a
terminal, and the command exits 0. --follow (-f) prints the last --tail
records (10 by default; --no-tail for the whole run first) and then keeps
the stream open, printing new records as the run makes them, until the run
ends: exit 0 done, 1 failed or abandoned, 3 waiting on a gate, 4 parked, 2
the stream itself broke.

The logs are redacted at write time. Full tool output is never written; a
summary line per tool result is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, opts, args[0])
		},
	}
	addReadFlags(cmd, &opts.readOptions)
	cmd.Flags().StringVar(&opts.step, "step", "", "narrow to one step (default: the whole run)")
	cmd.Flags().Int64Var(&opts.attempt, "attempt", 0, "narrow to one attempt of --step (default: every attempt)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "emit each record as an Activity JSON line")
	cmd.Flags().BoolVarP(&opts.follow, "follow", "f", false, "keep the stream open, printing new records as the run makes them")
	cmd.Flags().Int64VarP(&opts.tail, "tail", "n", 10, "with --follow, print this many records before going live")
	cmd.Flags().BoolVar(&opts.noTail, "no-tail", false, "with --follow, print the whole run before going live")
	cmd.Flags().BoolVar(&opts.noPager, "no-pager", false, "print directly to stdout instead of $PAGER")
	cmd.Flags().BoolVarP(&opts.end, "pager-end", "e", false, "open the pager at the end of the log")
	return cmd
}

func runLogs(cmd *cobra.Command, opts logsOptions, reference string) error {
	ctx := cmd.Context()
	store := storeClient(opts.socket)
	detail, err := inspected(ctx, store, "logs", reference)
	if err != nil {
		return err
	}
	known := newStepAttempts(detail.GetSteps())

	req := &millv1.LogsRequest{
		Id:      detail.GetSummary().GetRun().GetId(),
		Step:    opts.step,
		Attempt: opts.attempt,
		Follow:  opts.follow,
	}
	if opts.follow && !opts.noTail {
		req.Tail = opts.tail
	}

	stream, err := store.Logs(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("logs %s: %w", reference, err)
	}

	if opts.follow {
		if err := followLogs(cmd.OutOrStdout(), stream, opts, known); err != nil {
			return fmt.Errorf("logs %s: %w", reference, err)
		}
		return nil
	}

	content, err := readLogs(stream, opts, known)
	if err != nil {
		return fmt.Errorf("logs %s: %w", reference, err)
	}
	if err := page(ctx, cmd, opts, content); err != nil {
		return fmt.Errorf("logs %s: %w", reference, err)
	}
	return nil
}

// readLogs collects every record of an unfollowed stream, rendered, for
// page to show at once.
func readLogs(stream *connect.ServerStreamForClient[millv1.LogsResponse], opts logsOptions, known *stepAttempts) ([]byte, error) {
	var buf bytes.Buffer
	for stream.Receive() {
		if err := writeFrame(&buf, stream.Msg(), opts, known); err != nil {
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// followLogs prints every record as it streams, then the line naming how
// the run ended once a RunEnd frame arrives, or that the stream itself
// broke when it ends without one.
func followLogs(out io.Writer, stream *connect.ServerStreamForClient[millv1.LogsResponse], opts logsOptions, known *stepAttempts) error {
	for stream.Receive() {
		msg := stream.Msg()
		if end := msg.GetRun(); end != nil {
			return endLogs(out, end)
		}
		if err := writeFrame(out, msg, opts, known); err != nil {
			return err
		}
	}
	if err := stream.Err(); err != nil {
		if _, writeErr := fmt.Fprintf(out, "-- stream broke: %s --\n", err); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("%w: %s", errLogsStreamBroken, err)
	}
	// A step-scoped follow ends this way: the attempt left running and the
	// server closed the stream with no RunEnd, because only a whole-run
	// follow ends on the run itself (docs/activity.md §The RPC).
	return nil
}

// writeFrame renders one LogsResponse frame's activity, when it carries
// one, as --json wants it or as text.
func writeFrame(out io.Writer, msg *millv1.LogsResponse, opts logsOptions, known *stepAttempts) error {
	a := msg.GetActivity()
	if a == nil {
		return nil
	}
	known.observe(a)
	if opts.json {
		line, err := jsonLine(a)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, line)
		return err
	}
	line := renderLine(a, known)
	if line == "" {
		return nil
	}
	_, err := io.WriteString(out, line)
	return err
}

// jsonLine is a as the Activity JSON a script reads, one record per line.
func jsonLine(a *millv1.Activity) (string, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

// stepAttempt names the step and attempt number a step run id belongs to.
type stepAttempt struct {
	step    string
	attempt int64
}

// stepAttempts resolves an activity's step_run_id to the step and attempt
// it belongs to. attempt_started already carries its own step and attempt;
// attempt_ended does not, so its boundary line needs this looked up. Seeded
// from the run's steps as Inspect already returned them, and kept live as
// attempt_started records stream past — the seed alone would miss a step
// that starts after the stream opens, or one a --tail window opened after
// the start of.
type stepAttempts struct {
	byID map[string]stepAttempt
}

func newStepAttempts(steps []*millv1.StepRunDetail) *stepAttempts {
	known := make(map[string]stepAttempt, len(steps))
	for _, detail := range steps {
		sr := detail.GetStepRun()
		known[sr.GetId()] = stepAttempt{step: sr.GetStepName(), attempt: sr.GetAttempt()}
	}
	return &stepAttempts{byID: known}
}

func (s *stepAttempts) observe(a *millv1.Activity) {
	started := a.GetAttemptStarted()
	if started == nil {
		return
	}
	s.byID[a.GetStepRunId()] = stepAttempt{step: started.GetStep(), attempt: started.GetAttempt()}
}

func (s *stepAttempts) lookup(stepRunID string) stepAttempt {
	return s.byID[stepRunID]
}

// renderLine is one activity record the way `mill logs` prints it:
// attempt_started and attempt_ended become the open/close markers
// run.RenderActivity has no text for, since it renders the session's own
// events alone; everything else is RenderActivity's line.
func renderLine(a *millv1.Activity, known *stepAttempts) string {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_AttemptStarted:
		return fmt.Sprintf("-- %s attempt %d started --\n", body.AttemptStarted.GetStep(), body.AttemptStarted.GetAttempt())
	case *millv1.Activity_AttemptEnded:
		sa := known.lookup(a.GetStepRunId())
		return fmt.Sprintf("-- %s attempt %d ended: %s --\n", sa.step, sa.attempt, body.AttemptEnded.GetReason())
	default:
		return run.RenderActivity(a)
	}
}

// endLogs writes the line naming why a followed stream over one run ended
// and returns the error ExitCode reads the status from — nil for done, and
// otherwise one of errLogsWaiting, errLogsParked, or a plain error for
// failed/abandoned, which fall to the default exit status of 1.
func endLogs(out io.Writer, end *millv1.RunEnd) error {
	var line string
	var cause error
	switch end.GetStatus() {
	case millv1.RunStatus_RUN_STATUS_DONE:
		line = "-- run done --\n"
	case millv1.RunStatus_RUN_STATUS_FAILED:
		line, cause = "-- run failed --\n", fmt.Errorf("run %s: failed", end.GetRunId())
	case millv1.RunStatus_RUN_STATUS_ABANDONED:
		line, cause = "-- run abandoned --\n", fmt.Errorf("run %s: abandoned", end.GetRunId())
	case millv1.RunStatus_RUN_STATUS_WAITING:
		line = fmt.Sprintf("-- waiting on gate %s --\n", end.GetGate().GetNodeRef())
		cause = fmt.Errorf("%w: %s", errLogsWaiting, end.GetGate().GetNodeRef())
	case millv1.RunStatus_RUN_STATUS_PARKED:
		line = fmt.Sprintf("-- parked on gate %s --\n", end.GetGate().GetNodeRef())
		cause = fmt.Errorf("%w: %s", errLogsParked, end.GetGate().GetNodeRef())
	default:
		cause = fmt.Errorf("run %s: ended in unexpected status %s", end.GetRunId(), end.GetStatus())
	}
	if _, err := io.WriteString(out, line); err != nil {
		return err
	}
	return cause
}

// page writes content to stdout, piped through $PAGER when it is a real
// terminal and --no-pager was not given, or directly otherwise: a script
// reading --json output redirects stdout, which is exactly the case a
// pager would get in the way of.
func page(ctx context.Context, cmd *cobra.Command, opts logsOptions, content []byte) error {
	out := cmd.OutOrStdout()
	if opts.noPager || !isTerminal(out) {
		_, err := out.Write(content)
		return err
	}
	pager := pagerCommand()
	process, err := workspace.Spawner{}.Exec(ctx, workspace.Command{
		Path: pager,
		Args: pagerArgs(pager, opts.end),
		Env:  pagerEnv(),
	})
	if err != nil {
		return fmt.Errorf("start pager %s: %w", pager, err)
	}
	return runPager(process, content, out, cmd.ErrOrStderr())
}

// runPager feeds content to the pager's standard input, copies its output
// to out and its standard error to errOut, and waits for it to exit — the
// shape exec.Cmd.Run gives a caller for free when Stdin is a reader, done by
// hand because [workspace.Process] hands back pipes instead.
func runPager(process *workspace.Process, content []byte, out, errOut io.Writer) error {
	go func() {
		_, _ = process.Stdin.Write(content)
		_ = process.Stdin.Close()
	}()
	stdoutDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, process.Stdout)
		stdoutDone <- err
	}()
	_, _ = io.Copy(errOut, process.Stderr)
	if err := <-stdoutDone; err != nil {
		return err
	}
	result := process.Wait()
	if result.Status != 0 {
		return fmt.Errorf("pager exited %d", result.Status)
	}
	return nil
}

// isTerminal reports whether w is a terminal a pager should write to,
// rather than a file, a pipe, or a spec's buffer.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// pagerCommand is $PAGER, or less when it is unset — the pager git and
// journalctl fall back to.
func pagerCommand() string {
	if p := os.Getenv("PAGER"); p != "" {
		return p
	}
	return "less"
}

// pagerArgs is the pager's own arguments: --pager-end (-e) opens at the end
// of the log the way journalctl's -e does, which only less is told to
// understand — an arbitrary $PAGER is left alone rather than handed a flag
// it may not have.
func pagerArgs(pager string, end bool) []string {
	if end && filepath.Base(pager) == "less" {
		return []string{"+G"}
	}
	return nil
}

// pagerEnv is what this command adds to the pager's inherited environment:
// LESS=FRX when the operator has not set their own, so a log reads the way
// git's and journalctl's do — quit if it fits one screen, raw control
// characters for color, no screen-clear on exit.
func pagerEnv() map[string]string {
	if os.Getenv("LESS") != "" {
		return nil
	}
	return map[string]string{"LESS": "FRX"}
}

// defaultLogDir is where a worker under this account writes step logs. It
// is `mill run`'s, `mill work`'s, and `mill serve --work`'s default for
// --log-dir; `mill logs` does not take one, since the RPC it reads over
// answers from whichever host holds the record.
func defaultLogDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "mill", "logs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "mill", "logs")
	}
	return filepath.Join(home, ".local", "state", "mill", "logs")
}
