package run

import (
	"context"
	"fmt"
	"strings"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
)

// The circuit breaker: docs/architecture.md §"Failure and stop".
//
// A step whose retries are exhausted does not fail the run and does not try
// again. It posts what it has, comments every round of findings on the node,
// files a gate asking a human what to do, records the park, and releases the
// lease with reason `budget`. Asking a human costs the attempts it took to
// get there, and the human's two answers are `resume` — complete the gate,
// and the run continues at the parked step with its attempts reset — or
// `mill restart`.

// park trips the breaker.
func (r *Runner) park(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, _ *millv1.InspectResponse, cause error) error {
	rounds := s.findings[step.GetName()]
	if len(rounds) == 0 {
		rounds = []string{cause.Error()}
	}
	findings := roundsOf(step.GetName(), rounds)
	if err := r.options.Dagger.Comment(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName()+"-findings", sr.GetAttempt()), s.lease,
		s.node.GetReference(), findings); err != nil {
		return err
	}
	gate, err := r.options.Dagger.CreateGate(ctx,
		dagger.RequestID(s.run.GetId(), step.GetName()+"-park", sr.GetAttempt()), s.lease,
		dagger.Gate{
			Title:  fmt.Sprintf("%s: %s stopped after %d attempts", s.node.GetReference(), step.GetName(), sr.GetAttempt()),
			Body:   parkBody(s, step, findings),
			Blocks: s.node.GetReference(),
			Labels: dagger.GateLabels(s.run.GetId(), step.GetName()+"-park"),
		})
	if err != nil {
		return err
	}
	if _, err := call(ctx, r.options.Store.Park, &millv1.ParkRequest{
		LeaseId:    s.lease,
		RunId:      s.run.GetId(),
		GateNodeId: gate.GetId(),
		Reason:     millv1.ParkedReason_PARKED_REASON_RETRIES,
	}); err != nil {
		return err
	}
	if err := r.options.Dagger.Release(ctx, s.lease, daggerv1.ReleaseReason_RELEASE_REASON_BUDGET, "retries exhausted on "+step.GetName()); err != nil {
		return err
	}

	r.printf("  parked on %s; %s asks a human what to do\n", step.GetName(), gate.GetReference())
	return errReleased
}

// roundsOf is every round of findings in one comment, oldest first. One
// comment rather than one per round, because what a human is being asked to
// read is a sequence and reading it should not mean scrolling through a
// node's whole history to reassemble it.
func roundsOf(step string, rounds []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` failed %d times. Every round, oldest first:\n", step, len(rounds))
	for i, round := range rounds {
		fmt.Fprintf(&b, "\n### Round %d\n\n%s\n", i+1, strings.TrimSpace(round))
	}
	return b.String()
}

// parkBody is what the park gate asks.
func parkBody(s *state, step *millv1.Step, findings string) string {
	return fmt.Sprintf(
		"Run %s of %s stopped at step %s.\n\n%s\n\n"+
			"Complete this node to resume the run at that step with its attempts reset. "+
			"Drop it, or run `mill restart %s`, to start over.",
		s.run.GetId(), s.node.GetReference(), step.GetName(), findings, s.node.GetReference())
}

// resumePark is the other half of the breaker: a claim that took over a
// parked run reads the gate the breaker filed before it runs anything.
//
// A gate a human completed is the price paid, so the parked step's attempts
// so far no longer count against its retries and the run continues where it
// stopped. Anything else — an unanswered gate, or one disposed of rather
// than completed — leaves the run parked and gives the lease back: the
// human's other answer is `mill restart`, which is a different command and
// not something a worker decides for itself.
func (r *Runner) resumePark(ctx context.Context, s *state) error {
	if !s.parked {
		return nil
	}
	gate, err := r.gateByID(ctx, s, s.run.GetGateNodeId())
	if err != nil {
		return err
	}
	if gate.GetStatus() != daggerv1.Status_STATUS_DONE {
		if err := r.options.Dagger.Release(ctx, s.lease,
			daggerv1.ReleaseReason_RELEASE_REASON_GATE, "park gate "+gate.GetReference()); err != nil {
			return err
		}
		r.printf("park gate %s is unanswered; %s stays parked\n", gate.GetReference(), s.node.GetReference())
		return errReleased
	}

	detail, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
	if err != nil {
		return err
	}
	step := detail.GetSummary().GetCurrentStep()
	if sr := latestStepRun(detail, step); sr != nil {
		s.retried[step] = sr.GetAttempt()
	}
	if _, err := call(ctx, r.options.Store.Resume, &millv1.ResumeRequest{
		LeaseId: s.lease,
		RunId:   s.run.GetId(),
	}); err != nil {
		return err
	}
	s.parked = false
	r.printf("park gate %s was answered; resuming at %s with its attempts reset\n", gate.GetReference(), step)
	return nil
}
