package run_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// A worker stopped under a running step aborts the attempt, and the next
// worker starts a new one. The step did not fail — nobody answered it — so
// the abort must not spend one of the step's retries: a restart during a
// session costs the run time, never a round.

var _ = Describe("An attempt a worker's stop aborted", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("does not count against the step's retries", func() {
		By("given a gather-context step with retries 2 whose first attempt is aborted mid-turn")
		workflow := f.variant(ctx, "aborted-once", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 2
		})
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})
		const interval = time.Second
		runner := f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = 5 * time.Second
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0), "the session is in flight")
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
		Expect(stepRuns(f.inspect(ctx), "gather-context")[0].GetStatus()).
			To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))

		By("when the run resumes and the step then fails twice before it succeeds")
		f.script(append([]fake.Script{failed("round 1"), failed("round 2")}, f.quickFixScripts()...)...)
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the two failures are the retries, the abort is not, and the step closes on the fourth attempt")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(4))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[2].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[3].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(f.runOf(ctx).GetStatus()).NotTo(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(closedSteps(detail)).To(HaveKey("gather-context"))
	})
})

// The human's payment is the ledger's fact, not the worker's. A worker that
// takes the run over after the one that resumed it has gone must count the
// step's retries from the answered gate, exactly as that worker did.

var _ = Describe("A run taken over after a park gate was answered", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("counts the step's retries from the answer, not from the start", func() {
		By("given a gather-context step with retries 1 that parks after two failures")
		workflow := f.variant(ctx, "paid-once", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 1
		})
		f.script(failed("round 1"), failed("round 2"))
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))

		By("and a human answers the gate, and the worker that resumed it is stopped mid-attempt")
		f.approveGate(ctx, f.gatesOn(ctx)[0].GetReference())
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})
		const interval = time.Second
		runner := f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = 5 * time.Second
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0), "the session is in flight")
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))

		By("when a fresh worker takes the run over and the step fails once more before it succeeds")
		f.script(append([]fake.Script{failed("round 3")}, f.quickFixScripts()...)...)
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then that failure is the first retry since the answer, and the step closes")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(5))
		Expect(attempts[2].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
		Expect(attempts[3].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[4].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(f.runOf(ctx).GetStatus()).NotTo(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
	})
})
