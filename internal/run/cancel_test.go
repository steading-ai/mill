package run_test

import (
	"context"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
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
		// is given back as a preemption rather than a crash, and the run
		// keeps its workflow and its status.
		Expect(f.stepRunStatus(ctx, stepRunID)).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
		Expect(f.abortReason(ctx, stepRunID)).To(Equal("cancelled"))
		reason, detail := f.releasedLease(ctx)
		Expect(reason).To(Equal("preempted"))
		Expect(detail).To(Equal("cancel"))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
		Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the node is claimable again")
		Expect(f.runOf(ctx).GetLeaseId()).To(BeEmpty(),
			"the ledger lets go of the lease when skift does, as a drain's release does")
	})

	It("a cancelled run can be rerun before any worker claims it again", func() {
		// steading#17, 2026-09-29: a run cancelled so an operator could move
		// it back to its pr step was refused as "held under a live lease"
		// by the ledger, while skift showed the node unleased and no worker
		// held it; the only way on was to let a worker resume the step the
		// operator wanted to undo.
		f.node(ctx, "a step an operator wants back", "Cancelled, then rerun from its first step.")
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

		_, err := f.store.Rerun(ctx, connect.NewRequest(&millv1.RerunRequest{
			RunId:    f.runOf(ctx).GetId(),
			FromStep: "gather-context",
			Reason:   "the operator wants the step back",
		}))
		Expect(err).NotTo(HaveOccurred(), "a run no worker holds is the operator's to move")
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

	It("carries a revise's finding across a cancel that lands on the target's next attempt", func() {
		By("given a feature run whose review attempt 1 has just asked for changes, implement's second attempt in flight")
		f.pushWorkflowFrom(ctx, featureDir)
		f.node(ctx, "a fix cancelled mid-answer to a revise", "Change the README's first line.")
		writesPlan := func(harness.Session) {
			defer GinkgoRecover()
			Expect(f.workspace.Write(context.Background(), planPath(f.nodeRef),
				[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)))).To(Succeed())
		}
		f.script(
			ok("context gathered"),
			ok("explored the problem"),
			fake.Script{Terminal: fake.Terminal("planned", nil, usage(10)), Do: writesPlan},
			fake.Script{Terminal: fake.Terminal("", approves("AC-1", "the plan names its proof"), usage(5))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("simplified", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("", revises("AC-2", "needs another pass"), usage(5))},
			fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)},
		)

		runner := f.runner(func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = grace
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">=", 10), "implement's second attempt, answering the revise, is in flight")

		By("when cancel_requested is set")
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, interval+grace).Should(Receive(BeNil()))

		detailBefore := f.inspect(ctx)
		implementsBefore := stepRuns(detailBefore, "implement")
		Expect(implementsBefore).To(HaveLen(2), "round 1, superseded by the revise, and round 2, cancelled mid-turn")
		Expect(implementsBefore[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
		reviewAttempt1 := stepRuns(detailBefore, "review")[0]
		Expect(implementsBefore[1].GetFeedbackFrom()).To(Equal(reviewAttempt1.GetId()),
			"already durable at creation time; feedback_from was never what mill#100 lost")

		By("and the next worker resumes implement with review's finding as its prompt")
		f.script(
			fake.Script{Terminal: fake.Terminal("the change is made again", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("simplified again", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed again", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("opened the pull request again", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("", approves("AC-2", "the fix addresses the finding"), usage(5))},
		)
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		calls := f.harness.Calls()
		Expect(calls).NotTo(BeEmpty())
		Expect(calls[0].Prompt.Text).To(ContainSubstring("needs another pass"),
			"implement's third attempt is asked with review's finding, not nothing")

		implementsAfter := stepRuns(f.inspect(ctx), "implement")
		Expect(implementsAfter).To(HaveLen(3))
		Expect(implementsAfter[2].GetFeedbackFrom()).To(Equal(reviewAttempt1.GetId()))
	})
})
