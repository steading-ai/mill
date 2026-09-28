package run

import (
	"context"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/evidence"
)

// What happens to a failed attempt, in the order docs/domain-model.md §Step
// states it: the step's own retries first, then `on_failure` once they are
// spent — `revise <target>` sends the work back, `park` or nothing trips the
// circuit breaker (docs/ledger.md §Resume).
//
// `retries: 3` means three retries after the first try, and they are the
// step's own: attempts one through three ask the same step again, in the
// same session, with what the last attempt failed on as the next prompt.
// The fourth failure is the one that has nowhere left to go, and only then
// is `on_failure` consulted. What counts is measured from the last attempt a
// human paid for, so a run resumed out of a park gets a fresh set (see
// [state.attemptsAt]).
//
// A revise is consulted once per set of retries: the attempt that spends the
// last one supersedes the span, and a step that fails again with its retries
// already spent parks. Otherwise a step whose target cannot satisfy it would
// send the work back forever, and the breaker would never trip.
//
// One failure is not the step's own and skips the retries entirely: a
// reviewer's `revise` verdict. There the step judged, and its findings are
// addressed to the implementer; the retries bound the rounds of that
// (docs/domain-model.md §Verdict).

// failStep records a failed attempt and applies the step's retries, then its
// on_failure.
func (r *Runner) failStep(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, detail *millv1.InspectResponse, verified evidence.Evidence, res result, cause error) (outcome, error) {
	exit := res.exit
	if exit == nil {
		exit = &harnessProcessExit
	}
	// A reviewer's verdict never closes the attempt it fails, but it is
	// still recorded on it: Inspect returns a revise the same way it
	// returns an approve (docs/architecture.md §Evidence).
	var failEvidence *millv1.Evidence
	if res.judgment {
		failEvidence = verified.Value
	}
	if _, err := call(ctx, r.options.Store.FailStepRun, &millv1.FailStepRunRequest{
		LeaseId:   s.leaseID(),
		StepRunId: sr.GetId(),
		Exit:      exit,
		Evidence:  failEvidence,
	}); err != nil {
		return outcome{}, err
	}
	findings := res.feedback
	if findings == "" {
		findings = cause.Error()
	}
	if err := r.endActivity(ctx, s, step, sr, millv1.StepRunStatus_STEP_RUN_STATUS_FAILED, millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED, findings); err != nil {
		return outcome{}, err
	}
	// Every round is kept, not only the last: what a human needs when the
	// breaker finally trips is the sequence, because three rounds of the
	// same finding say something one round does not.
	s.record(step.GetName(), findings)
	s.recordAttempt(step, sr, exit, lastWords(res, findings), true)
	r.printf("  failed: %v\n", cause)

	attempts := s.attemptsAt(detail, step.GetName(), sr.GetAttempt())
	retries := int64(step.GetRetries())
	target := step.GetOnFailure().GetReviseTarget()
	revising := step.GetOnFailure().GetKind() == millv1.OnFailureKind_ON_FAILURE_KIND_REVISE && target != ""

	// A reviewer's `revise` verdict is not the step's own failure. The step
	// judged, and the judgment is about another step's work: asking the
	// same reviewer again would be asking it to change its mind, and its
	// findings are addressed to the implementer. It goes to `on_failure` at
	// once, and the step's retries bound how many rounds of that the run
	// gets (docs/domain-model.md §Verdict, §Revise).
	if res.judgment {
		if revising && attempts <= retries {
			return r.revise(ctx, s, step, sr, target, findings)
		}
		return outcome{}, r.park(ctx, s, step, sr, detail, cause)
	}

	if attempts <= retries {
		// The step's own attempt, again. Nothing is superseded and no other
		// step is implicated: this failure is the step's to answer, and the
		// session it ran in is asked once more with the failure as its next
		// prompt.
		//
		// A session whose structured output did not parse judged nothing,
		// which is the same answer arrived at differently: there is no
		// verdict, so there is nothing to send anybody, and the same
		// reviewer is told what its output was missing rather than an
		// implementer's attempt being spent on a failure that was the
		// reviewer's own (docs/domain-model.md §Verdict).
		if evidence.Malformed(cause) {
			r.printf("  malformed output; asking %s again\n", step.GetName())
		} else {
			r.printf("  retrying %s\n", step.GetName())
		}
		return outcome{feedback: findings, feedbackFrom: sr.GetId()}, nil
	}

	// Malformed output never revises, whatever the step says: nothing was
	// judged, so no other step's work is implicated (docs/domain-model.md
	// §Verdict). The run parks when the step's retries are spent.
	if revising && !evidence.Malformed(cause) && attempts == retries+1 {
		return r.revise(ctx, s, step, sr, target, findings)
	}
	return outcome{}, r.park(ctx, s, step, sr, detail, cause)
}

// lastWords is what this attempt said for itself: the session's final
// message, the check's stderr, or — when it produced neither — the mill's
// own account of why the attempt failed.
func lastWords(res result, findings string) string {
	if res.session.FinalMessage != "" {
		return res.session.FinalMessage
	}
	if res.stderr != "" {
		return res.stderr
	}
	return findings
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
		LeaseId:     s.leaseID(),
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
