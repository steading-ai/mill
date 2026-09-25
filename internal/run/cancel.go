package run

import (
	"context"
	"errors"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// Cancellation is what `mill cancel` asks of a running worker, and the whole
// of what the worker does about it.
//
// docs/architecture.md §Cancel: the CLI sets `cancel_requested` on the run
// and writes nothing else. The worker reads the flag on every heartbeat tick
// — the one place polling is allowed, because there is no channel from the
// service to a worker and inventing one would be a second scheduler. Reading
// it raises [errCancelled], which cancels the step's context; the adapter
// stops the session with SIGTERM, a grace window, then SIGKILL; and
// [Runner.cancelStep] finishes the job:
//
//   - the open attempt is aborted with reason `cancelled`, so the ledger says
//     why it ended rather than leaving a row a resume would call a crash;
//   - the lease is released `preempted` with detail `cancel`, so dagger's
//     event log distinguishes a node taken back on purpose from one whose
//     worker died;
//   - nothing else is written. The run keeps its status, its workflow, and
//     its branch, so the next claim resumes at the same step, one attempt
//     further on.
//
// A cancelled run is therefore not a failed one. It is a run that stopped,
// and `mill run` on the same node continues it.

// errCancelled is a cancellation a human asked for. The step is aborted, the
// lease released with preempted, and the run stays resumable at that step.
var errCancelled = errors.New("run: cancelled")

// cancelStep is what a cancellation does to the run: the attempt is aborted,
// the lease is released with preempted, and the run is left resumable.
//
// It is called with a context the heartbeat has not cancelled — the step's
// context is already dead by the time this runs — because these two writes
// are the point of the cancellation and must land.
func (r *Runner) cancelStep(ctx context.Context, s *state) error {
	if _, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
		Reason:  "cancelled",
	}); err != nil {
		return err
	}
	if err := r.options.Dagger.Release(ctx, s.leaseID(), skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "cancel"); err != nil {
		return err
	}
	r.printf("cancelled; run %s stays resumable\n", s.run.GetId())
	return errReleased
}
