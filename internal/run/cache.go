package run

import (
	"context"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/workspace"
)

// cacheCandidate is the step-close hook for cache lineages: once a step has
// closed on evidence, the workspace's cache copies are snapshotted as the
// run's promotion candidate, replacing the previous step's. It is called
// from closeStep and nowhere else, before anything stops the step's slice
// — the machine backend's step boundary follows it (docs/sandbox.md §"Step
// boundary") — so every path that ends a step without evidence leaves the
// candidate as it was.
//
// A candidate that cannot be taken is logged and does not fail the step:
// the evidence is recorded, the run goes on, and the run promotes from the
// last candidate it did take, or nothing.
func (r *Runner) cacheCandidate(ctx context.Context, s *state, step *millv1.Step) {
	// A workspace with a step boundary takes the candidate in EndStep,
	// before it stops the step's slice (docs/sandbox.md §"Step boundary").
	// This hook is for a backend that carries lineages without one.
	if _, ok := s.workspace.(workspace.StepBoundary); ok {
		return
	}
	candidates, ok := s.workspace.(workspace.CacheCandidate)
	if !ok {
		return
	}
	if err := candidates.Candidate(ctx, step.GetName()); err != nil {
		r.options.Log.Warn("cache candidate not taken", "run", s.run.GetId(), "step", step.GetName(), "error", err)
		return
	}
	r.printf("  cache candidate taken at %s\n", step.GetName())
}
