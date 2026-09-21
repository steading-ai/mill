package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/run"
)

// `mill logs` reads the per-attempt activity records the worker writes
// through the redactor as a session's events arrive (docs/activity.md), and
// renders them back into the lines an operator reads. The redaction
// happened at write time, so this command has nothing to scrub: it decodes
// and renders. A value a session registered with the `redact` verb was
// masked before it reached the file, and there is no unredacted copy
// anywhere for `--follow` to race with.

// logPoll is how often --follow looks for more bytes and asks the ledger
// whether the attempt is still running.
const logPoll = 200 * time.Millisecond

func newLogsCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "logs <run>",
		Short: "Read a step run's redacted log",
		Long: `Read the log of one attempt at one step: the session's events, one summary
line each, as the worker wrote them through the redactor.

With no --step this is the step the run is on. --follow tails the file and
returns when the attempt leaves running, so a session watching a step knows
the step ended rather than guessing from silence.

The logs are redacted at write time. Full tool output is never written; a
summary line per tool result is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, opts, args[0])
		},
	}
	addReadFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.step, "step", "", "the step to read (default: the step the run is on)")
	cmd.Flags().BoolVar(&opts.follow, "follow", false, "tail the log until the attempt leaves running")
	cmd.Flags().StringVar(&opts.logDir, "log-dir", defaultLogDir(), "directory the worker writes step logs under")
	return cmd
}

func runLogs(cmd *cobra.Command, opts readOptions, reference string) error {
	store := storeClient(opts.socket)
	detail, err := inspected(cmd.Context(), store, "logs", reference)
	if err != nil {
		return err
	}
	step := opts.step
	if step == "" {
		step = currentOrLastStep(detail)
	}
	if step == "" {
		return fmt.Errorf("logs %s: the run has attempted no step; there is nothing to read", reference)
	}
	attempt := latestAttempt(detail.GetSteps(), step)
	if attempt == nil {
		return fmt.Errorf("logs %s: no attempt at step %q has been made", reference, step)
	}
	runID := detail.GetSummary().GetRun().GetId()
	path := run.ActivityPath(opts.logDir, runID, step, attempt.GetStepRun().GetAttempt())

	if !opts.follow {
		activities, err := run.ReadActivities(opts.logDir, runID, step, attempt.GetStepRun().GetAttempt())
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("logs %s: no log for %s attempt %d at %s", reference, step,
				attempt.GetStepRun().GetAttempt(), path)
		}
		if err != nil {
			return fmt.Errorf("logs %s: %w", reference, err)
		}
		return renderActivities(cmd.OutOrStdout(), activities)
	}

	file, err := os.Open(path) //nolint:gosec // the path is built from the ledger's own ids
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("logs %s: no log for %s attempt %d at %s", reference, step,
			attempt.GetStepRun().GetAttempt(), path)
	}
	if err != nil {
		return fmt.Errorf("logs %s: %w", reference, err)
	}
	defer func() { _ = file.Close() }()
	return follow(cmd.Context(), cmd.OutOrStdout(), file, store, attempt.GetStepRun().GetId(), runID)
}

// renderActivities writes every activity's rendered line to out, in order,
// skipping a record the log never showed a line for (docs/activity.md's
// "Today" column — a verb, a gate filing, an attempt's own boundary).
func renderActivities(out io.Writer, activities []*millv1.Activity) error {
	for _, a := range activities {
		if line := run.RenderActivity(a); line != "" {
			if _, err := io.WriteString(out, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// currentOrLastStep is the step this tool reads with no --step: the step the
// run is on, or — for a run that has ended, and so is on none — the last step
// it attempted.
//
// A run that finished is exactly the run whose logs somebody wants: the
// alternative is answering "the run is on no step" to a question about the
// only run the node has, which is a tool refusing to read a file it can name.
func currentOrLastStep(detail *millv1.InspectResponse) string {
	if current := detail.GetSummary().GetCurrentStep(); current != "" {
		return current
	}
	steps := detail.GetSteps()
	if len(steps) == 0 {
		return ""
	}
	return steps[len(steps)-1].GetStepRun().GetStepName()
}

// follow tails the log until the attempt leaves running.
//
// The order matters: the file is drained, and only then is the ledger asked
// whether the attempt is still running. A tail that asked first would exit
// having missed the lines written between the two, which is exactly the
// window a step closing carries its last words in.
func follow(ctx context.Context, out io.Writer, file *os.File, store millv1connect.StoreServiceClient, stepRunID, runID string) error {
	for {
		if err := drainActivities(out, file); err != nil {
			return err
		}
		running, err := stillRunning(ctx, store, runID, stepRunID)
		if err != nil {
			return err
		}
		if !running {
			// One last drain: the writes that closed the attempt may have
			// landed between the drain above and the ledger's answer.
			return drainActivities(out, file)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(logPoll):
		}
	}
}

// drainActivities writes every complete record file currently holds, then
// stops: a trailing partial record — the tail of one still being written —
// is not yet a record, so the read leaves it for the next poll rather than
// treating a few new bytes as something to show.
func drainActivities(out io.Writer, file *os.File) error {
	for {
		a, ok, err := run.ReadActivity(file)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if line := run.RenderActivity(a); line != "" {
			if _, err := io.WriteString(out, line); err != nil {
				return err
			}
		}
	}
}

// stillRunning asks the ledger whether one attempt is still open.
func stillRunning(ctx context.Context, store millv1connect.StoreServiceClient, runID, stepRunID string) (bool, error) {
	res, err := store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: runID}))
	if err != nil {
		return false, fmt.Errorf("logs: %w", err)
	}
	for _, step := range res.Msg.GetSteps() {
		if step.GetStepRun().GetId() == stepRunID {
			return step.GetStepRun().GetStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, nil
		}
	}
	return false, nil
}

// defaultLogDir is where a worker under this account writes step logs.
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
