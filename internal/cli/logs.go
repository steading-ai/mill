package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/workspace"
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
		Use:   "logs [run]",
		Short: "Read a run's activity, live or in full",
		Long: `Read a run's activity: every attempt of every step, oldest first, rendered
the way the session's events read as a log. With no --step this is the
whole run; --step narrows to one step, --attempt to one attempt of it.

With no run, every run's activity is merged instead, oldest first, each
line prefixed with the run it belongs to. --since narrows to records at or
after a point in time, a duration ago (1h, 7d) or an absolute timestamp; a
fresh --follow with no --since starts from now rather than replaying the
ledger's whole history.

With no --follow the whole answer is printed once, through $PAGER on a
terminal, and the command exits 0. --follow (-f) prints the last --tail
records (10 by default; --no-tail for the whole run first) and then keeps
the stream open, printing new records as the run makes them. Named a run,
it ends when that run does: exit 0 done, 1 failed or abandoned, 3 waiting
on a gate, 4 parked, 2 the stream itself broke. With no run it never ends
on its own — ctrl-C exits 0, a broken stream still exits 2.

The logs are redacted at write time. Full tool output is never written; a
summary line per tool result is.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var reference string
			if len(args) > 0 {
				reference = args[0]
			}
			return runLogs(cmd, opts, reference)
		},
	}
	addReadFlags(cmd, &opts.readOptions)
	cmd.Flags().StringVar(&opts.step, "step", "", "narrow to one step (default: the whole run)")
	cmd.Flags().Int64Var(&opts.attempt, "attempt", 0, "narrow to one attempt of --step (default: every attempt)")
	cmd.Flags().StringVar(&opts.since, "since", "", "with no run, only records at or after this point: a duration ago (1h, 7d) or an absolute timestamp")
	cmd.Flags().BoolVar(&opts.json, "json", false, "emit each record as an Activity JSON line")
	cmd.Flags().BoolVarP(&opts.follow, "follow", "f", false, "keep the stream open, printing new records as the run makes them")
	cmd.Flags().Int64VarP(&opts.tail, "tail", "n", 10, "with --follow, print this many records before going live")
	cmd.Flags().BoolVar(&opts.noTail, "no-tail", false, "with --follow, print the whole run before going live")
	cmd.Flags().BoolVar(&opts.noPager, "no-pager", false, "print directly to stdout instead of $PAGER")
	cmd.Flags().BoolVarP(&opts.end, "pager-end", "e", false, "open the pager at the end of the log")
	return cmd
}

// refText is reference as an error message names it: a run, or "every run"
// for the no-run stream.
func refText(reference string) string {
	if reference == "" {
		return "every run"
	}
	return reference
}

func runLogs(cmd *cobra.Command, opts logsOptions, reference string) error {
	ctx := cmd.Context()
	store := storeClient(opts.socket)

	var req *millv1.LogsRequest
	var lines *logsRenderer
	var names *runNames

	if reference == "" {
		since, err := sinceBound(opts, time.Now())
		if err != nil {
			return err
		}
		req = &millv1.LogsRequest{Step: opts.step, Attempt: opts.attempt, Follow: opts.follow}
		if since != nil {
			req.Since = timestamppb.New(*since)
		}
		lines = newLogsRenderer(nil, opts.follow)
		names = newRunNames(ctx, store)
	} else {
		detail, err := inspected(ctx, store, "logs", reference)
		if err != nil {
			return err
		}
		lines = newLogsRenderer(detail.GetSteps(), opts.follow)
		req = &millv1.LogsRequest{
			Id:      detail.GetSummary().GetRun().GetId(),
			Step:    opts.step,
			Attempt: opts.attempt,
			Follow:  opts.follow,
		}
	}
	if opts.follow && !opts.noTail {
		req.Tail = opts.tail
	}

	// An every-run follow never ends on its own (the RPC's own contract) —
	// ctrl-C is the only way this process is meant to stop it, so the
	// context it streams over must actually hear the signal.
	if opts.follow && reference == "" {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}

	stream, err := store.Logs(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("logs %s: %w", refText(reference), err)
	}

	if opts.follow {
		if err := followLogs(ctx, cmd.OutOrStdout(), stream, opts, lines, names); err != nil {
			return fmt.Errorf("logs %s: %w", refText(reference), err)
		}
		return nil
	}

	content, err := readLogs(stream, opts, lines, names)
	if err != nil {
		return fmt.Errorf("logs %s: %w", refText(reference), err)
	}
	if err := page(ctx, cmd, opts, content); err != nil {
		return fmt.Errorf("logs %s: %w", refText(reference), err)
	}
	return nil
}

// sinceBound is the lower bound a no-run stream applies: --since parsed
// literally when given; otherwise no bound for a plain read (every record on
// file, the same as naming a run), or now for a fresh --follow (mill#16's
// Decisions) — so a follow with no run does not open by replaying the
// ledger's entire history before going live.
func sinceBound(opts logsOptions, now time.Time) (*time.Time, error) {
	if opts.since != "" {
		t, err := parseSince(opts.since, now)
		if err != nil {
			return nil, err
		}
		return &t, nil
	}
	if opts.follow {
		return &now, nil
	}
	return nil, nil
}

// parseSince reads --since the way journalctl's own does: a duration ago
// (1h, 7d, via parseWindow) or an absolute timestamp, RFC3339 or the
// "2006-01-02 15:04:05" / "2006-01-02" forms journalctl also accepts,
// read in the local timezone since neither carries one of its own.
func parseSince(text string, now time.Time) (time.Time, error) {
	if window, err := parseWindow(text); err == nil {
		return now.Add(-window), nil
	}
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since: %q is not a duration (1h, 7d) or a timestamp", text)
}

// readLogs collects every record of an unfollowed stream, rendered, for
// page to show at once.
func readLogs(stream *connect.ServerStreamForClient[millv1.LogsResponse], opts logsOptions, lines *logsRenderer, names *runNames) ([]byte, error) {
	var buf bytes.Buffer
	for stream.Receive() {
		if err := writeFrame(&buf, stream.Msg(), opts, lines, names); err != nil {
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
// broke when it ends without one. An every-run follow sends no RunEnd
// (docs/activity.md: "over every run it does not end") — it reads to here
// only once ctx itself ends, either ctrl-C's ctx.Err() (a clean stop, not a
// broken stream) or the stream failing on its own.
func followLogs(ctx context.Context, out io.Writer, stream *connect.ServerStreamForClient[millv1.LogsResponse], opts logsOptions, lines *logsRenderer, names *runNames) error {
	for stream.Receive() {
		msg := stream.Msg()
		if end := msg.GetRun(); end != nil {
			return endLogs(out, end)
		}
		if err := writeFrame(out, msg, opts, lines, names); err != nil {
			return err
		}
	}
	if err := stream.Err(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
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
// one, as --json wants it or as text. names is nil for a single-run stream,
// which never prefixes a line with a run it is already scoped to.
func writeFrame(out io.Writer, msg *millv1.LogsResponse, opts logsOptions, lines *logsRenderer, names *runNames) error {
	a := msg.GetActivity()
	if a == nil {
		return nil
	}
	lines.known.observe(a)
	if opts.json {
		line, err := jsonLine(a)
		if err != nil {
			return err
		}
		_, err = io.WriteString(out, line)
		return err
	}
	line := lines.render(a)
	if line == "" {
		return nil
	}
	if names != nil {
		line = prefixLines(names.prefix(a.GetStepRunId()), line)
	}
	_, err := io.WriteString(out, line)
	return err
}

// prefixLines leads every line of text with prefix, not only its first —
// docs/activity.md's prompt and check bodies render as several lines from
// one record, and mill#16's every-run stream names which run each one of
// them is from, not just the first.
func prefixLines(prefix, text string) string {
	if prefix == "" {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
	}
	return b.String()
}

// runNames resolves an activity's step_run_id to the node_ref of the run it
// belongs to, for the no-run stream's line prefix. The wire carries only
// step_run_id — a live file and a sealed frame hold the same record either
// way, and a run's identity is not one of its own facts — so this builds
// the mapping from the two calls Inspect already exposes rather than the
// server growing a new field for it (mill#16's Decisions names both and
// leaves the choice to this step; the CLI-side lookup needs no wire
// change).
//
// Seeded lazily and refreshed whenever a step_run_id it does not yet know
// turns up: a new run, or a new attempt of one already known, appearing
// mid-follow is exactly that case. A run Status or Inspect cannot presently
// reach leaves that step_run_id unprefixed rather than failing the stream.
type runNames struct {
	ctx   context.Context
	store millv1connect.StoreServiceClient
	byID  map[string]string
}

func newRunNames(ctx context.Context, store millv1connect.StoreServiceClient) *runNames {
	return &runNames{ctx: ctx, store: store, byID: map[string]string{}}
}

// prefix is stepRunID's run, ready to lead a rendered line, or empty when
// it cannot be resolved.
func (r *runNames) prefix(stepRunID string) string {
	if name, ok := r.byID[stepRunID]; ok {
		return name + " "
	}
	r.refresh()
	if name := r.byID[stepRunID]; name != "" {
		return name + " "
	}
	return ""
}

func (r *runNames) refresh() {
	res, err := r.store.Status(r.ctx, connect.NewRequest(&millv1.StatusRequest{Statuses: domain.EveryRunStatus}))
	if err != nil {
		return
	}
	for _, summary := range res.Msg.GetRuns() {
		run := summary.GetRun()
		detail, err := r.store.Inspect(r.ctx, connect.NewRequest(&millv1.InspectRequest{Id: run.GetId()}))
		if err != nil {
			continue
		}
		for _, sd := range detail.Msg.GetSteps() {
			r.byID[sd.GetStepRun().GetId()] = run.GetNodeRef()
		}
	}
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

// logsRenderer is one activity record the way `mill logs` prints it:
// attempt_started and attempt_ended become the open/close markers
// run.RenderActivity has no text for, since it renders the session's own
// events alone; everything else is the ActivityRenderer's line, so a Text
// or Thinking block that streamed as deltas renders as the one line it
// would have if it had arrived whole. Under --follow a streamed block's
// pieces — the records carrying its block id, delta or final — print raw
// instead, with no summary line, so a tail shows words appearing as the
// model writes them rather than waiting for the block to close
// (docs/activity.md §"Streaming text"). A whole Text with no block id is
// not a piece of anything and still renders as its line.
type logsRenderer struct {
	known  *stepAttempts
	blocks *run.ActivityRenderer
	follow bool
}

func newLogsRenderer(steps []*millv1.StepRunDetail, follow bool) *logsRenderer {
	return &logsRenderer{known: newStepAttempts(steps), blocks: run.NewActivityRenderer(), follow: follow}
}

func (r *logsRenderer) render(a *millv1.Activity) string {
	switch body := a.GetBody().(type) {
	case *millv1.Activity_AttemptStarted:
		return fmt.Sprintf("-- %s attempt %d started --\n", body.AttemptStarted.GetStep(), body.AttemptStarted.GetAttempt())
	case *millv1.Activity_AttemptEnded:
		sa := r.known.lookup(a.GetStepRunId())
		return fmt.Sprintf("-- %s attempt %d ended: %s --\n", sa.step, sa.attempt, body.AttemptEnded.GetReason())
	case *millv1.Activity_Text:
		if r.follow && body.Text.GetBlockId() != "" {
			return body.Text.GetText()
		}
	case *millv1.Activity_Thinking:
		if r.follow && body.Thinking.GetBlockId() != "" {
			return body.Thinking.GetText()
		}
	}
	return r.blocks.Render(a)
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
