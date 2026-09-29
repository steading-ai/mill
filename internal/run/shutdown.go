package run

import (
	"context"
	"errors"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// Stopping a worker, from the run's side.
//
// docs/architecture.md §"Failure and stop": on SIGTERM the worker stops
// claiming, lets the current step reach a boundary if it can inside the
// grace window, posts unposted usage, releases every lease with reason
// `preempted` and detail `shutdown` — skift's ReleaseReason has no shutdown
// value, and an operator taking the lease away is what preempted means —
// and leaves workspaces intact.
//
// That is a cancellation in every respect but who asked for it and what the
// event log records, so it is the same mechanism: the heartbeat raises
// [errShutdown], the step's context is cancelled, the adapter stops the
// session, and [Runner.shutdownStep] writes the facts that make the run resumable. "Resume is the crash path" is the whole of the recovery: the
// next claim finds a `running` run with an aborted attempt and starts the
// next attempt at the same step, in the same session.

// errShutdown is the process going away. The step is aborted, the lease
// released with preempted and detail `shutdown`, and the run stays resumable
// at that step.
var errShutdown = errors.New("run: shutting down")

// shutdownStep leaves the run where the next worker can pick it up.
//
// It is called with a context the heartbeat has not cancelled, because these
// writes are the point of the shutdown and must land.
func (r *Runner) shutdownStep(ctx context.Context, s *state) error {
	if _, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
		Reason:  "shutdown",
	}); err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.ReleaseLease, &millv1.ReleaseLeaseRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
	}); err != nil {
		return err
	}
	if err := r.options.Skift.Release(ctx, s.leaseID(),
		skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "shutdown"); err != nil {
		return err
	}
	r.printf("shutting down; run %s stays resumable\n", s.run.GetId())
	return errReleased
}

// drainStep gives the run's lease back at a step boundary, deliberately not
// calling [Runner.shutdownStep]: nothing is open, so there is nothing for
// AbortOpenStepRuns to abort. Like shutdownStep, it ends the loop by
// returning errReleased: the run stays running with no lease
// (docs/ledger.md §Resume), and nothing downstream needs to tell a drain's
// release apart from any other.
func (r *Runner) drainStep(ctx context.Context, s *state) error {
	if _, err := call(ctx, r.options.Store.ReleaseLease, &millv1.ReleaseLeaseRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
	}); err != nil {
		return err
	}
	if err := r.options.Skift.Release(ctx, s.leaseID(),
		skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "drain"); err != nil {
		return err
	}
	r.printf("draining; run %s stays resumable\n", s.run.GetId())
	return errReleased
}
