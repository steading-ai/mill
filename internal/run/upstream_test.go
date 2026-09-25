package run_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// UpstreamWait, UpstreamStreak and UpstreamGiveUp are mill#57's whole policy
// for an outage, specified directly against their own inputs: an
// InspectResponse built by hand for UpstreamStreak, since that is the one
// fact a fresh worker or `mill explain` has to recover the streak from
// (Invariant 10), and plain durations for the other two.

var base = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func inspectWith(entries ...*millv1.StepRun) *millv1.InspectResponse {
	steps := make([]*millv1.StepRunDetail, 0, len(entries))
	for _, sr := range entries {
		steps = append(steps, &millv1.StepRunDetail{StepRun: sr})
	}
	return &millv1.InspectResponse{Steps: steps}
}

func refusedAttempt(step string, attempt int64, at time.Time) *millv1.StepRun {
	return &millv1.StepRun{
		StepName: step, Attempt: attempt, EndedAt: timestamppb.New(at),
		Exit: &harnessv1.ProcessExit{Reason: harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED},
	}
}

func closedAttempt(step string, attempt int64, at time.Time) *millv1.StepRun {
	return &millv1.StepRun{StepName: step, Attempt: attempt, EndedAt: timestamppb.New(at)}
}

var _ = Describe("UpstreamWait", Label("unit"), func() {
	It("doubles per consecutive refusal, capped at five minutes", func() {
		Expect(run.UpstreamWait(1)).To(Equal(15 * time.Second))
		Expect(run.UpstreamWait(2)).To(Equal(30 * time.Second))
		Expect(run.UpstreamWait(3)).To(Equal(time.Minute))
		Expect(run.UpstreamWait(4)).To(Equal(2 * time.Minute))
		Expect(run.UpstreamWait(5)).To(Equal(4 * time.Minute))
		Expect(run.UpstreamWait(6)).To(Equal(5 * time.Minute))
		Expect(run.UpstreamWait(20)).To(Equal(5 * time.Minute))
	})
})

var _ = Describe("UpstreamStreak", Label("unit"), func() {
	It("is zero when the step's latest attempt was not a refusal", func() {
		detail := inspectWith(
			refusedAttempt("gather-context", 1, base),
			closedAttempt("gather-context", 2, base.Add(time.Minute)),
		)
		count, since := run.UpstreamStreak(detail, "gather-context")
		Expect(count).To(BeZero())
		Expect(since.IsZero()).To(BeTrue())
	})

	It("counts the trailing run of refusals and when the first of them ended", func() {
		detail := inspectWith(
			closedAttempt("gather-context", 1, base),
			refusedAttempt("gather-context", 2, base.Add(5*time.Minute)),
			refusedAttempt("gather-context", 3, base.Add(7*time.Minute)),
		)
		count, since := run.UpstreamStreak(detail, "gather-context")
		Expect(count).To(Equal(int64(2)))
		Expect(since).To(BeTemporally("==", base.Add(5*time.Minute)))
	})

	It("only counts the named step's attempts", func() {
		detail := inspectWith(
			refusedAttempt("gather-context", 1, base),
			refusedAttempt("implement", 1, base),
		)
		count, since := run.UpstreamStreak(detail, "implement")
		Expect(count).To(Equal(int64(1)))
		Expect(since).To(BeTemporally("==", base))
	})
})

var _ = Describe("UpstreamGiveUp", Label("unit"), func() {
	It("is false with no streak", func() {
		Expect(run.UpstreamGiveUp(time.Time{}, time.Now())).To(BeFalse())
	})

	It("is false short of thirty minutes and true at or past it", func() {
		Expect(run.UpstreamGiveUp(base, base.Add(29*time.Minute))).To(BeFalse())
		Expect(run.UpstreamGiveUp(base, base.Add(30*time.Minute))).To(BeTrue())
		Expect(run.UpstreamGiveUp(base, base.Add(45*time.Minute))).To(BeTrue())
	})
})

// The end-to-end path: a step whose upstream refuses it a few times waits
// and asks again on its own, spending none of the step's retries, and an
// outage that outlasts the wait parks once with the outage named as the
// reason, not the same round reported over and over (mill#57's scenario).

var _ = Describe("A step the upstream refuses", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("waits out a short outage for free and closes on its own evidence", func() {
		By("given a gather-context step with retries 1 whose upstream refuses it twice")
		workflow := f.variant(ctx, "upstream-outage", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 1
		})
		var mu sync.Mutex
		var waits []time.Duration
		f.script(append([]fake.Script{refused("refused 1"), refused("refused 2")}, f.quickFixScripts()...)...)

		By("when the run is executed")
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.Sleep = func(_ context.Context, d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				waits = append(waits, d)
			}
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the refusals waited an escalating delay and cost the step nothing")
		mu.Lock()
		Expect(waits).To(Equal([]time.Duration{15 * time.Second, 30 * time.Second}))
		mu.Unlock()

		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(3))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[0].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[1].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		Expect(attempts[2].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(f.runOf(ctx).GetStatus()).NotTo(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(closedSteps(detail)).To(HaveKey("gather-context"))
	})

	It("parks once on the outage when it outlasts the wait, not the same round three times", func() {
		By("given a gather-context step whose upstream refuses it for over thirty minutes")
		workflow := f.variant(ctx, "upstream-giveup", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 5
		})

		var mu sync.Mutex
		clock := time.Now()
		advance := func(d time.Duration) func(harness.Session) {
			return func(harness.Session) {
				mu.Lock()
				defer mu.Unlock()
				clock = clock.Add(d)
			}
		}
		now := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		}
		refusedTurn := func(final string, do func(harness.Session)) fake.Script {
			return fake.Script{
				Terminal: &harness.Terminal{
					FinalMessage: final,
					Exit:         harness.ProcessExit{Status: 1, Reason: harness.ReasonUpstream},
					Usage:        usage(1),
				},
				Do: do,
			}
		}
		f.script(
			refusedTurn("refused 1", advance(10*time.Minute)),
			refusedTurn("refused 2", advance(10*time.Minute)),
			refusedTurn("refused 3", advance(15*time.Minute)),
		)

		By("when the run is executed")
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.Now = now
			o.Sleep = func(context.Context, time.Duration) {}
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then every refusal is on the ledger and the run parks, retries untouched")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(3))
		for _, a := range attempts {
			Expect(a.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
			Expect(a.GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		}
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))

		By("and the human sees the outage once, not the same round repeated")
		comments := f.comments(ctx)
		Expect(comments).To(HaveLen(1), "one comment, not one per attempt")
		Expect(comments[0]).To(ContainSubstring("failed 1 times"), "one round, not three")
		Expect(comments[0]).To(ContainSubstring("outlasted the wait"))
		Expect(comments[0]).NotTo(ContainSubstring("Round 2"))
	})
})
