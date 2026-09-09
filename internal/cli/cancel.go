package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// `mill cancel` is the one operate tool this milestone builds, and it is a
// request rather than a transition: it sets `cancel_requested` on the run and
// writes nothing else (docs/architecture.md §Cancel). The worker reads the
// flag at its next heartbeat, stops the step through the adapter, aborts the
// attempt with reason `cancelled`, releases its lease `preempted` with detail
// `cancel`, and leaves the run resumable at that step.
//
// The caller holds no lease, so nothing fences this write. What guards it is
// the socket's peer uid: the role map admits `operator` and the service's own
// account, refuses `reader` naming the role, and refuses a uid it does not
// name as unauthenticated (docs/architecture.md §"Identity and credentials").

func newCancelCommand() *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "cancel <run>",
		Short: "Ask a run's worker to stop cleanly at its next heartbeat",
		Long: `Ask the worker holding a run to stop the step it is on.

This sets a flag; the worker acts on it. Within one heartbeat interval plus
the step's grace period the session is stopped, the attempt is recorded
aborted with reason "cancelled", and the lease is released as preempted.

The run is not failed and not abandoned: it keeps its workflow and its
branch, and "mill run" on the same node resumes at that step with the next
attempt. Use "mill restart" to throw the work away instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCancel(cmd, opts, args[0])
		},
	}
	cmd.Flags().StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	cmd.Flags().StringVar(&opts.reason, "reason", "", "why, recorded on the run's audit trail")
	return cmd
}

func runCancel(cmd *cobra.Command, opts readOptions, reference string) error {
	store := storeClient(opts.socket)
	// Cancel takes a run id and the operator has a node reference as often
	// as not, so the id is resolved first through the read every other tool
	// uses.
	detail, err := store.Inspect(cmd.Context(), connect.NewRequest(target(reference)))
	if err != nil {
		return fmt.Errorf("cancel %s: %w", reference, err)
	}
	run := detail.Msg.GetSummary().GetRun()
	res, err := store.Cancel(cmd.Context(), connect.NewRequest(&millv1.CancelRequest{
		RunId: run.GetId(), Reason: opts.reason,
	}))
	if err != nil {
		return fmt.Errorf("cancel %s: %w", reference, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"cancel requested on %s run %s at step %s; the worker stops at its next heartbeat\n",
		run.GetNodeRef(), res.Msg.GetRun().GetId(), dash(detail.Msg.GetSummary().GetCurrentStep()))
	return nil
}
