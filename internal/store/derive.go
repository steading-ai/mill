package store

import (
	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// RunState is a run together with the latest attempt at each of its steps:
// everything [CurrentStep] derives from, and nothing more.
type RunState struct {
	// Run is the run itself.
	Run *millv1.Run
	// Latest is the newest attempt at each step, by step name. A step with
	// no attempt has no entry.
	Latest map[string]*millv1.StepRun
}

// CurrentStep is the first step in the workflow whose latest step run is
// not closed — no row at all, or a latest row that is running, failed,
// aborted, or superseded. It returns the step's name and how many attempts
// have been made at it, which is what the circuit breaker compares against
// the step's retries.
//
// This is docs/ledger.md's derivation exactly, and it is a derivation
// rather than a column for the reason stated there: a stored current step
// would drift from the step runs, which is the failure the ledger exists to
// prevent (Invariant 10).
//
// A revise is what moves the current step backwards: superseding the latest
// attempt at every step from the revise target through the failing step
// leaves the target as the first step that is not closed, while the
// evidence those attempts produced stays in place and closes nothing.
//
// An empty name means every step is closed: the run's terminal step has
// evidence and the run is ready to complete. A run with no workflow yet is
// also empty, because it has no steps to be at.
func CurrentStep(run RunState, workflow *millv1.Workflow) (name string, attempts int64) {
	if workflow == nil {
		return "", 0
	}
	for _, step := range workflow.GetSteps() {
		latest, ok := run.Latest[step.GetName()]
		if !ok {
			return step.GetName(), 0
		}
		if latest.GetStatus() != millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED {
			return step.GetName(), latest.GetAttempt()
		}
	}
	return "", 0
}
