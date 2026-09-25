package run_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
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

			// When the review has spent everything it has. `retries: 3` is
			// three retries after the first try, so attempts one to three
			// are the step's own; the fourth failure is the one with
			// nowhere left to go and sends the work back to implement, and
			// the fifth — its retries already spent — is the breaker's.
			Expect(f.runner(withHarness(adapter), func(o *run.Options) {
				o.Workflow = workflow
				o.Skift = client
			}).Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			Expect(stepRuns(detail, "review")).To(HaveLen(5))

			// The revise happened once, after the fourth failure and not
			// before: implement ran a second time, answering that attempt,
			// and the three before it were the step asking itself again.
			reviews := stepRuns(detail, "review")
			implements := stepRuns(detail, "implement")
			Expect(implements).To(HaveLen(2))
			Expect(implements[1].GetFeedbackFrom()).To(Equal(reviews[3].GetId()),
				"the revise answers the fourth failure")
			for i := 1; i < 4; i++ {
				Expect(reviews[i].GetFeedbackFrom()).To(Equal(reviews[i-1].GetId()),
					"attempt %d answers the one before it", i+1)
			}

			// Then the run is parked.
			parked := f.runOf(ctx)
			Expect(parked.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
			Expect(parked.GetParkedReason()).To(Equal(millv1.ParkedReason_PARKED_REASON_RETRIES))
			Expect(parked.GetGateNodeId()).To(Equal(f.theGate(ctx).GetId()))
			Expect(watcher.reasons()).To(ContainElement(skiftv1.ReleaseReason_RELEASE_REASON_BUDGET))

			// And the human sees the rounds of findings in one comment.
			comments := f.comments(ctx)
			Expect(comments).To(HaveLen(1), "one comment, not one per round")
			for _, round := range []string{
				"round 1: the change still does the wrong thing",
				"round 2: the change still does the wrong thing",
				"round 3: the change still does the wrong thing",
				"round 4: the change still does the wrong thing",
				"round 5: the change still does the wrong thing",
			} {
				Expect(comments[0]).To(ContainSubstring(round))
			}
		})

		It("resumes at the parked step with its attempts reset once the gate is answered", func() {
			workflow := f.reviewedFix(ctx, reviewModel)
			adapter := &reviewer{model: reviewModel, commits: f.commits}
			runner := f.runner(withHarness(adapter), func(o *run.Options) { o.Workflow = workflow })
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())
			Expect(stepRuns(f.inspect(ctx), "review")).To(HaveLen(5))

			// The human's answer is what pays for the next set of attempts.
			f.approveGate(ctx, f.gatesOn(ctx)[0].GetReference())
			Expect(runner.Execute(ctx, f.nodeRef)).To(Succeed())

			// Five more, not one: the attempt count the breaker compares
			// against retries starts again from the answer, for the parked
			// step and no other.
			Expect(stepRuns(f.inspect(ctx), "review")).To(HaveLen(10))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		})
	})

	Describe("A check that sends the work back", func() {
		It("supersedes the target and everything after it, and runs the target again", func() {
			// clean-tree's on_failure is `revise commit`, so a dirty tree
			// sends the session back to the step that stages, not to
			// implement — and a check gets no retries of its own by
			// default, so the very first failure is the one with nowhere
			// left to go.
			workflow := f.variant(ctx, "dirty-fix", func(definition *millv1.Workflow) {
				stepOf(definition, "clean-tree").GetCheck().Command = []string{"false"}
			})
			f.script(f.quickFixScripts()...)

			Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
				Execute(ctx, f.nodeRef)).To(Succeed())

			detail := f.inspect(ctx)
			// implement is not implicated: the revise target is commit, not
			// implement, so implement's one attempt stands.
			Expect(stepRuns(detail, "implement")).To(HaveLen(1))
			Expect(stepRuns(detail, "implement")[0].GetStatus()).
				To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

			commits := stepRuns(detail, "commit")
			Expect(commits).To(HaveLen(2))
			Expect(commits[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
			Expect(commits[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

			// The check's own attempt failed and, having no retries of its
			// own, spent immediately: that first attempt is the one the
			// revise superseded. Commit runs again, and clean-tree runs a
			// second time on the tree it left — still dirty, because the
			// script never fixed it — and that second failure has nowhere
			// left to go: the revise-once rule means it parks rather than
			// revising commit a second time.
			checks := stepRuns(detail, "clean-tree")
			Expect(checks).To(HaveLen(2))
			Expect(checks[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))
			Expect(checks[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))

			Expect(commits[1].GetFeedbackFrom()).To(Equal(checks[0].GetId()),
				"the revise answers the check's failure")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		})
	})
})
