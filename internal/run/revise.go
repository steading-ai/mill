package run

import (
	"context"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// What happens to a failed attempt: `on_failure: revise <target>` while the
// step has attempts left, the circuit breaker when it does not
// (docs/domain-model.md §Step, docs/ledger.md §Resume).
//
// Retries are counted on the failing step's own attempts, and `retries: 3`
// means three retries after the first try: attempts one through three send
// the work back, and the fourth failure parks. What counts is measured from
// the last attempt a human paid for, so a run resumed out of a park gets a
// fresh set (see [state.attemptsAt]).

// failStep records a failed attempt and applies the step's on_failure.
func (r *Runner) failStep(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, res result, cause error) (outcome, error) {
	exit := res.exit
	if exit == nil {
		exit = &harnessProcessExit
	}
	if _, err := call(ctx, r.options.Store.FailStepRun, &millv1.FailStepRunRequest{
		LeaseId:   s.lease,
		StepRunId: sr.GetId(),
		Exit:      exit,
	}); err != nil {
		return outcome{}, err
	}
	findings := res.feedback
	if findings == "" {
		findings = cause.Error()
	}
	// Every round is kept, not only the last: what a human needs when the
	// breaker finally trips is the sequence, because three rounds of the
	// same finding say something one round does not.
	s.record(step.GetName(), findings)
	r.printf("  failed: %v\n", cause)

	target := step.GetOnFailure().GetReviseTarget()
	revising := step.GetOnFailure().GetKind() == millv1.OnFailureKind_ON_FAILURE_KIND_REVISE && target != ""
	if revising && s.attemptsAt(step.GetName(), sr.GetAttempt()) <= int64(step.GetRetries()) {
		return r.revise(ctx, s, step, sr, target, findings)
	}
	return outcome{}, r.park(ctx, s, step, sr, detail, cause)
}

// revise sends the work back to the step that produced it. Every step from
// the revise target through the failing step is superseded, so the revised
// work is re-committed, re-checked and re-reviewed; the evidence those
// attempts produced stays and closes nothing (docs/ledger.md, "revise
// actually revises"). The current step is derived from the ledger on the
// next pass, so superseding those rows is what moves the loop backwards —
// nothing is carried in the worker's head (Invariant 10).
func (r *Runner) revise(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, target, findings string) (outcome, error) {
	if _, err := call(ctx, r.options.Store.SupersedeSteps, &millv1.SupersedeStepsRequest{
		LeaseId:     s.lease,
		RunId:       s.run.GetId(),
		FromStep:    target,
		ThroughStep: step.GetName(),
	}); err != nil {
		return outcome{}, err
	}
	r.printf("  revising from %s\n", target)
	// The feedback is carried to the target's session as the next prompt,
	// and the attempt it answers is named on the new step run so the ledger
	// says which failure this pass is for.
	return outcome{feedback: findings, feedbackFrom: sr.GetId()}, nil
}

// harnessProcessExit is the exit recorded for a failure with no process
// behind it: the mill could not establish the fact, and nothing ran.
var harnessProcessExit = neverRan()
