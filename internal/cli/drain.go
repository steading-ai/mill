package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
)

// `mill drain` is the operate act that stops a worker cleanly: it asks the
// worker behind the socket to stop claiming and let every run it already
// holds reach its next step boundary, release its lease preempted with
// detail "drain", and stay resumable — no attempt aborted.
//
// The call itself is fire-and-forget: WorkerService.Drain returns as soon
// as the worker has been asked, not once it is done. --wait is this
// command's own patience for that: it polls Probe until the worker reports
// idle or --timeout elapses.

// DefaultDrainTimeout bounds `mill drain --wait`: long enough for one
// in-flight step to reach a boundary under an ordinary workflow's step
// budgets, short enough that an operator's terminal command does not hang
// indefinitely on a run that is stuck.
const DefaultDrainTimeout = 5 * time.Minute

// drainPollInterval is how often `--wait` asks Probe for the worker's state.
const drainPollInterval = time.Second

type drainOptions struct {
	socket  string
	wait    bool
	timeout time.Duration
}

func newDrainCommand() *cobra.Command {
	opts := drainOptions{}
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Ask the local worker to stop claiming and drain every run in flight",
		Long: `Ask the worker behind this socket to stop claiming new work and let every
run it already holds reach its next step boundary before releasing the
lease, with no attempt aborted. The run stays resumable at that step.

This returns as soon as the worker has been asked; it does not wait for the
worker to actually finish. Pass --wait to block until "mill status"'s
underlying Probe reports the worker idle, or --timeout elapses first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDrain(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	f.BoolVar(&opts.wait, "wait", false, "block until the worker reports idle, or --timeout elapses")
	f.DurationVar(&opts.timeout, "timeout", DefaultDrainTimeout, "with --wait, how long to wait for the worker to reach idle")
	return cmd
}

func runDrain(cmd *cobra.Command, opts drainOptions) error {
	client := workerClient(opts.socket)
	if _, err := client.Drain(cmd.Context(), connect.NewRequest(&millv1.DrainRequest{})); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "drain requested; the worker stops claiming and releases each run in flight at its next step boundary")
	if !opts.wait {
		return nil
	}
	return waitForIdle(cmd.Context(), out, client, opts.timeout)
}

// waitForIdle polls Probe until the worker reports idle or timeout elapses,
// naming the timeout rather than surfacing a bare context error when it
// does, since that is the one outcome `mill drain --wait` promises to
// report distinctly. It also prints each run's release as it drops out of
// Probe's in-flight list, and, on timeout, names whatever is still there.
func waitForIdle(ctx context.Context, out io.Writer, client millv1connect.WorkerServiceClient, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	var inFlight []*millv1.InFlightRun
	timedOut := func() error {
		return fmt.Errorf("drain --wait: worker did not reach idle within %s: still in flight: %s",
			timeout, describeInFlight(inFlight))
	}
	for {
		if ctx.Err() != nil {
			return timedOut()
		}
		res, err := client.Probe(ctx, connect.NewRequest(&millv1.ProbeRequest{}))
		if err != nil {
			return fmt.Errorf("drain --wait: %w", err)
		}
		reportReleased(out, inFlight, res.Msg.GetInFlight())
		inFlight = res.Msg.GetInFlight()
		if res.Msg.GetState() == millv1.WorkerState_WORKER_STATE_IDLE {
			_, _ = fmt.Fprintln(out, "worker is idle; drain complete")
			return nil
		}
		select {
		case <-ctx.Done():
			return timedOut()
		case <-ticker.C:
		}
	}
}

// reportReleased prints a line for each run present in before but no longer
// in after: a run whose lease was released, its step boundary closed,
// between the two polls that produced them.
func reportReleased(out io.Writer, before, after []*millv1.InFlightRun) {
	still := make(map[string]bool, len(after))
	for _, run := range after {
		still[run.GetRunId()] = true
	}
	for _, run := range before {
		if !still[run.GetRunId()] {
			_, _ = fmt.Fprintf(out, "released %s (%s)\n", run.GetRunId(), run.GetNodeRef())
		}
	}
}

// describeInFlight names the runs `--wait` is still waiting on, for a
// timeout error.
func describeInFlight(runs []*millv1.InFlightRun) string {
	if len(runs) == 0 {
		return "none reported"
	}
	names := make([]string, len(runs))
	for i, run := range runs {
		names[i] = fmt.Sprintf("%s (%s)", run.GetRunId(), run.GetNodeRef())
	}
	return strings.Join(names, ", ")
}
