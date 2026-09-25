package domain

import (
	"fmt"
	"strings"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// runTransitions is the state diagram in docs/domain-model.md §"Run states",
// written out. An edge that is not drawn there is not here, and a move along
// one is refused with both states named.
//
//	selecting ──▶ running ──▶ done
//	                │  ▲
//	                │  └──── waiting
//	                │  ▲
//	                │  └──── parked
//	                ├──────▶ failed
//	                └──────▶ abandoned
//
// done, failed and abandoned are terminal and have no outgoing edges.
// abandoned is reachable from every live state, because a restart applies to
// the live run whatever it is doing and a run stopped at a gate is the
// ordinary thing to restart. failed is reachable from every live state too:
// a gate is denied under a lease, but a node is closed underneath a run only
// while the run holds none — waiting, parked, or selecting with its claim
// gone — and the sweep that records it finds the run wherever it stopped
// (docs/ledger.md §Sweep).
//
// One write is deliberately not governed by this table: SetLease, the
// takeover a new claim performs. The proto and docs/ledger.md Invariant 9
// give it its own admission rule — running with an expired lease, waiting,
// parked, or done with the node still open — because it is the write that
// changes what later writes are fenced by. The store owns that rule; see
// [Service.SetLease].
var runTransitions = map[millv1.RunStatus]map[millv1.RunStatus]bool{
	millv1.RunStatus_RUN_STATUS_SELECTING: {
		millv1.RunStatus_RUN_STATUS_RUNNING:   true,
		millv1.RunStatus_RUN_STATUS_FAILED:    true,
		millv1.RunStatus_RUN_STATUS_ABANDONED: true,
	},
	millv1.RunStatus_RUN_STATUS_RUNNING: {
		millv1.RunStatus_RUN_STATUS_DONE:      true,
		millv1.RunStatus_RUN_STATUS_WAITING:   true,
		millv1.RunStatus_RUN_STATUS_PARKED:    true,
		millv1.RunStatus_RUN_STATUS_FAILED:    true,
		millv1.RunStatus_RUN_STATUS_ABANDONED: true,
	},
	millv1.RunStatus_RUN_STATUS_WAITING: {
		millv1.RunStatus_RUN_STATUS_RUNNING:   true,
		millv1.RunStatus_RUN_STATUS_FAILED:    true,
		millv1.RunStatus_RUN_STATUS_ABANDONED: true,
	},
	millv1.RunStatus_RUN_STATUS_PARKED: {
		millv1.RunStatus_RUN_STATUS_RUNNING:   true,
		millv1.RunStatus_RUN_STATUS_FAILED:    true,
		millv1.RunStatus_RUN_STATUS_ABANDONED: true,
	},
}

// RunStatuses is every run status in the model, terminal ones included. The
// transition table is exhaustive over it.
func RunStatuses() []millv1.RunStatus {
	return []millv1.RunStatus{
		millv1.RunStatus_RUN_STATUS_SELECTING,
		millv1.RunStatus_RUN_STATUS_RUNNING,
		millv1.RunStatus_RUN_STATUS_WAITING,
		millv1.RunStatus_RUN_STATUS_PARKED,
		millv1.RunStatus_RUN_STATUS_DONE,
		millv1.RunStatus_RUN_STATUS_FAILED,
		millv1.RunStatus_RUN_STATUS_ABANDONED,
	}
}

// RunTransitionError is a move the diagram does not draw. It names both
// states, because "invalid transition" without them tells a worker nothing
// about what it found.
type RunTransitionError struct {
	From, To millv1.RunStatus
}

func (e *RunTransitionError) Error() string {
	return fmt.Sprintf("run is %s: a %s run cannot become %s",
		RunStatusName(e.From), RunStatusName(e.From), RunStatusName(e.To))
}

// CheckRunTransition admits from → to when the diagram draws that edge and
// refuses it otherwise.
func CheckRunTransition(from, to millv1.RunStatus) error {
	if runTransitions[from][to] {
		return nil
	}
	return &RunTransitionError{From: from, To: to}
}

// RunEndedError is an operation on a run that has ended. Distinct from a
// transition: nothing is being moved, the run is simply over.
type RunEndedError struct {
	Status millv1.RunStatus
}

func (e *RunEndedError) Error() string {
	return fmt.Sprintf("run is %s and has ended", RunStatusName(e.Status))
}

// stepRunTransitions is the smaller diagram of docs/ledger.md §step_runs. An
// attempt is opened running and ends once: closed by evidence, failed so
// on_failure applies, or aborted because the worker stopped or was fenced
// out. A revise supersedes whatever the latest attempt ended as, which is
// what moves the run's current step backwards; a superseded attempt is the
// record of a pass that is over and moves no further.
var stepRunTransitions = map[millv1.StepRunStatus]map[millv1.StepRunStatus]bool{
	millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING: {
		millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED:  true,
		millv1.StepRunStatus_STEP_RUN_STATUS_FAILED:  true,
		millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED: true,
	},
	millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED: {
		millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED: true,
	},
	millv1.StepRunStatus_STEP_RUN_STATUS_FAILED: {
		millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED: true,
	},
	millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED: {
		millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED: true,
	},
}

// StepRunTransitionError is a move the step run diagram does not draw.
type StepRunTransitionError struct {
	From, To millv1.StepRunStatus
}

func (e *StepRunTransitionError) Error() string {
	return fmt.Sprintf("step run is %s: a %s attempt cannot become %s",
		StepRunStatusName(e.From), StepRunStatusName(e.From), StepRunStatusName(e.To))
}

// CheckStepRunTransition admits from → to for one attempt.
func CheckStepRunTransition(from, to millv1.StepRunStatus) error {
	if stepRunTransitions[from][to] {
		return nil
	}
	return &StepRunTransitionError{From: from, To: to}
}

// RunStatusName is the status as the model and the ledger spell it: the
// enum value without its prefix, lowered.
func RunStatusName(status millv1.RunStatus) string {
	return enumName(status.String(), "RUN_STATUS_")
}

// StepRunStatusName is the attempt status as the ledger spells it.
func StepRunStatusName(status millv1.StepRunStatus) string {
	return enumName(status.String(), "STEP_RUN_STATUS_")
}

// EvidenceKindName is the evidence kind as the model spells it.
func EvidenceKindName(kind millv1.EvidenceKind) string {
	return enumName(kind.String(), "EVIDENCE_KIND_")
}

func enumName(value, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(value, prefix))
}
