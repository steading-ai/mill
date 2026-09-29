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

var _ = Describe("Cancelling a run", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and the run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	// The interval and the grace period are the whole of the bound the
	// design puts on a cancellation: the flag is noticed at the next
	// heartbeat, and the step has the grace window to stop after that
	// (docs/architecture.md §Cancel).
	const interval = time.Second
	const grace = 5 * time.Second

	It("Cancel is honoured at the next heartbeat", func() {
		By("given a step that will run for ten minutes")
		// A held turn ends only when the session is cancelled: for the
		// worker it is indistinguishable from a session that would run for
		// ten minutes, and it cannot end on its own and pass this spec by
		// accident.
		f.node(ctx, "a long step", "This one would run for ten minutes.")
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})

		runner := f.runner(func(o *run.Options) {
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = grace
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0), "the step is in flight")
		stepRunID := f.openStepRun(ctx).GetId()

		By("when cancel_requested is set")
		Expect(f.cancelRun(ctx)).To(Succeed())

		By("then the step ends within one heartbeat interval plus the grace period")
		Eventually(done, interval+grace).Should(Receive(BeNil()))

		By("and the run is resumable at that step")
		// Resumable means three things, and the ledger and skift hold all
		// three: the attempt is aborted rather than left running, the lease
		// is given back as a preemption rather than a crash, and the run is
		// left `stopped` rather than `running` with no worker behind it.
		Expect(f.stepRunStatus(ctx, stepRunID)).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
		Expect(f.abortReason(ctx, stepRunID)).To(Equal("cancelled"))
		reason, detail := f.releasedLease(ctx)
		Expect(reason).To(Equal("preempted"))
		Expect(detail).To(Equal("cancel"))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))
		Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the node is claimable again")
	})

	It("resumes at the cancelled step with attempt 2 when the node is run again", func() {
		f.node(ctx, "a cancelled step", "This one is stopped and then finished.")
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})

		runner := f.runner(func(o *run.Options) {
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = grace
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0))
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, interval+grace).Should(Receive(BeNil()))

		// No expiry is forced here, unlike the crash spec: a cancellation
		// hands the lease back, so the node is claimable at once.
		f.script(f.quickFixScripts()...)
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		attempts := map[int64]millv1.StepRunStatus{}
		for _, step := range f.inspect(ctx).GetSteps() {
			if step.GetStepRun().GetStepName() == "gather-context" {
				attempts[step.GetStepRun().GetAttempt()] = step.GetStepRun().GetStatus()
			}
		}
		Expect(attempts).To(Equal(map[int64]millv1.StepRunStatus{
			1: millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED,
			2: millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED,
		}))
		Expect(f.runOf(ctx).GetCancelRequested()).To(BeFalse(),
			"the flag was cleared when the run was taken over")
	})
})
