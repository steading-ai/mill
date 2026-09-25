package run

import (
	"context"
	"fmt"
	"time"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// mill#57: every request a turn made refused by the upstream — 529, 503,
// unreachable — is an outage, not the step's own work. It carries no verdict
// evidence to consult, so it never reaches [Runner.verify]; it does not spend
// the step's retries, so [state.attemptsAt] discounts it the way an aborted
// attempt already is; and it is answered by waiting and trying again, with a
// delay that grows with how many refusals have come in a row.
//
// UpstreamWait, UpstreamStreak and UpstreamGiveUp are that policy's whole
// arithmetic, exported and pure so internal/cli computes the same wait from
// the same ledger that the worker retrying the step does (Invariant 10:
// nothing is carried in the worker's head).

// upstreamBaseWait is the delay after the first consecutive refusal.
const upstreamBaseWait = 15 * time.Second

// upstreamMaxWait caps the delay so a long outage is retried every five
// minutes rather than falling further and further behind it.
const upstreamMaxWait = 5 * time.Minute

// upstreamGiveUpAfter is how long a step may wait out an outage before the
// breaker trips: past this, a human is told once rather than the run
// retrying silently forever.
const upstreamGiveUpAfter = 30 * time.Minute

// UpstreamWait is how long to wait before the next attempt after consecutive
// refusals in a row: 15s, 30s, 1m, doubling, capped at 5m.
func UpstreamWait(consecutive int64) time.Duration {
	wait := upstreamBaseWait
	for i := int64(1); i < consecutive; i++ {
		wait *= 2
		if wait >= upstreamMaxWait {
			return upstreamMaxWait
		}
	}
	return wait
}

// UpstreamStreak is the trailing run of consecutive upstream refusals at
// step, and when the first of them ended: how long the run has been waiting
// out this outage. Zero and the zero time when the step's latest attempt was
// not itself a refusal, so an ordinary failure — or a close — ends the streak
// exactly as it ends the run's wait.
//
// Steps arrive in ledger order (attempts increase as it goes), the same
// assumption [latestAttempt] makes, so walking from the end needs no sort.
func UpstreamStreak(detail *millv1.InspectResponse, step string) (count int64, since time.Time) {
	steps := detail.GetSteps()
	for i := len(steps) - 1; i >= 0; i-- {
		sr := steps[i].GetStepRun()
		if sr.GetStepName() != step {
			continue
		}
		if sr.GetExit().GetReason() != harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED {
			break
		}
		count++
		since = sr.GetEndedAt().AsTime()
	}
	return count, since
}

// UpstreamGiveUp reports whether an outage a step has been waiting out is old
// enough that mill#57's own bound applies: since is the first attempt of the
// streak (UpstreamStreak), and thirty minutes of consecutive refusals is
// treated as an outage a human should be told about once rather than one the
// run keeps waiting out.
func UpstreamGiveUp(since, now time.Time) bool {
	return !since.IsZero() && now.Sub(since) >= upstreamGiveUpAfter
}

// upstreamRefusal is what the step run does with an attempt every request of
// which the upstream refused: it is an outage, not a verdict to ask about, so
// the caller reaches this instead of [Runner.verify]. The attempt's exit —
// carrying [harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED] — is recorded
// first, because it is the fact [UpstreamStreak] and [state.attemptsAt] both
// read back off the ledger; then the run either waits out the outage and
// retries the same step, or, past [upstreamGiveUpAfter] of consecutive
// refusals, tells a human once rather than retrying silently forever.
func (r *Runner) upstreamRefusal(ctx context.Context, s *state, step *millv1.Step, sr *millv1.StepRun, res result, cause error) (outcome, error) {
	if _, err := call(ctx, r.options.Store.FailStepRun, &millv1.FailStepRunRequest{
		LeaseId:   s.leaseID(),
		StepRunId: sr.GetId(),
		Exit:      res.exit,
	}); err != nil {
		return outcome{}, err
	}

	fresh, err := call(ctx, r.options.Store.Inspect, &millv1.InspectRequest{Id: s.run.GetId()})
	if err != nil {
		return outcome{}, err
	}
	count, since := UpstreamStreak(fresh, step.GetName())
	now := r.options.Now()

	if UpstreamGiveUp(since, now) {
		reason := fmt.Sprintf("the upstream refused every request for %d attempts since %s; that has outlasted the wait, so a human is being asked",
			count, since.UTC().Format(time.RFC3339))
		if err := r.endActivity(ctx, s, step, sr, millv1.StepRunStatus_STEP_RUN_STATUS_FAILED, millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED, reason); err != nil {
			return outcome{}, err
		}
		// One finding for the whole streak, not one per refusal: the park
		// gate is meant to say this was an outage, once, not the same
		// round three times (mill#57).
		s.record(step.GetName(), reason)
		s.recordAttempt(step, sr, res.exit, lastWords(res, reason), true)
		r.printf("  upstream refused %s for %s; parked\n", step.GetName(), now.Sub(since).Round(time.Second))
		return outcome{}, r.park(ctx, s, step, sr, fresh, cause)
	}

	wait := UpstreamWait(count)
	reason := fmt.Sprintf("the upstream refused every request; waiting %s before attempt %d", wait, count+1)
	if err := r.endActivity(ctx, s, step, sr, millv1.StepRunStatus_STEP_RUN_STATUS_FAILED, millv1.EvidenceKind_EVIDENCE_KIND_UNSPECIFIED, reason); err != nil {
		return outcome{}, err
	}
	r.printf("  upstream refused %s; retrying in %s\n", step.GetName(), wait)
	r.wait(ctx, wait)
	if err := ctx.Err(); err != nil {
		return outcome{}, r.stopped(ctx, s)
	}
	return outcome{}, nil
}

// wait blocks for d or until ctx ends, whichever comes first, through
// [Options.Sleep] when a spec has set one: the escalating delay a real
// outage earns should not be a real delay a test has to sit through, and
// [UpstreamWait]'s own arithmetic is what both a worker and `mill explain`
// compute it from either way.
func (r *Runner) wait(ctx context.Context, d time.Duration) {
	if r.options.Sleep != nil {
		r.options.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
