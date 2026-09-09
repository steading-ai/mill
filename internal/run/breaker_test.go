package run_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("The circuit breaker", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and the run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	Describe("A step that keeps failing", func() {
		const reviewModel = "always-unhappy"

		It("Escalation has a price", func() {
			// Given review fails with revise implement on every attempt.
			workflow := f.reviewedFix(ctx, reviewModel)
			adapter := &reviewer{model: reviewModel, commits: f.commits}
			client, watcher := f.watching()

			// When the fourth failure occurs. `retries: 3` is three retries
			// after the first try, so attempts one to three send the work
			// back and the fourth is the one that cannot.
			Expect(f.runner(withHarness(adapter), func(o *run.Options) {
				o.Workflow = workflow
				o.Dagger = client
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "review")).To(HaveLen(4))

			// Then the run is parked.
			parked := f.runOf(ctx)
			Expect(parked.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
			Expect(parked.GetParkedReason()).To(Equal(millv1.ParkedReason_PARKED_REASON_RETRIES))
			Expect(parked.GetGateNodeId()).To(Equal(f.theGate(ctx).GetId()))
			Expect(watcher.reasons()).To(ContainElement(daggerv1.ReleaseReason_RELEASE_REASON_BUDGET))

			// And the human sees the rounds of findings in one comment.
			comments := f.comments(ctx)
			Expect(comments).To(HaveLen(1), "one comment, not one per round")
			for _, round := range []string{
				"round 1: the change still does the wrong thing",
				"round 2: the change still does the wrong thing",
				"round 3: the change still does the wrong thing",
			} {
				Expect(comments[0]).To(ContainSubstring(round))
			}
		})

		It("resumes at the parked step with its attempts reset once the gate is answered", func() {
			workflow := f.reviewedFix(ctx, reviewModel)
			adapter := &reviewer{model: reviewModel, commits: f.commits}
			runner := f.runner(withHarness(adapter), func(o *run.Options) { o.Workflow = workflow })
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())
			Expect(stepRuns(f.inspect(ctx), "review")).To(HaveLen(4))

			// The human's answer is what pays for the next set of attempts.
			f.approveGate(ctx, f.gatesOn(ctx)[0].GetReference())
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())

			// Four more, not one: the attempt count the breaker compares
			// against retries starts again from the answer.
			Expect(stepRuns(f.inspect(ctx), "review")).To(HaveLen(8))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		})
	})

	Describe("A check that sends the work back", func() {
		It("supersedes the target and everything after it, and runs the target again", func() {
			// clean-tree's on_failure is `revise implement`, so a dirty
			// tree sends the session back to the step that made it.
			workflow := f.variant(ctx, "dirty-fix", func(definition *millv1.Workflow) {
				stepOf(definition, "clean-tree").GetCheck().Command = []string{"false"}
			})
			f.script(f.quickFixScripts()...)

			Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "implement")[0].GetStatus()).
				To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
			Expect(stepRuns(detail, "commit")[0].GetStatus()).
				To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
			Expect(stepRuns(detail, "clean-tree")[0].GetStatus()).
				To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))

			// The current step went backwards to the revise target, which
			// the ledger says by what ran next.
			Expect(after(detail, "clean-tree")).To(Equal("implement"))
			Expect(stepRuns(detail, "implement")).To(HaveLen(4))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		})
	})
})
