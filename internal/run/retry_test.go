package run_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

// Retries are the step's own: docs/domain-model.md §Step, "retries first,
// then on_failure". A step that fails is asked again, in the same session,
// with what the last attempt failed on as its next prompt; nothing is
// superseded and no other step is implicated. Only when the attempts are
// spent is `on_failure` read at all.
//
// And what the breaker files when they are spent has to carry the span:
// the step that parks is regularly not the step that caused it.

var _ = Describe("Retries are the step's own", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("A step retries itself before anything else happens", func() {
		By("given a skill step with retries 2 and no on_failure")
		const reviewModel = "twice-unhappy"
		workflow := f.retriedReview(ctx, reviewModel, 2, nil)

		By("when its session fails twice and succeeds on the third attempt")
		adapter := &reviewer{model: reviewModel, commits: f.commits, until: 2}
		Expect(f.runner(withHarness(adapter), func(o *run.Options) { o.Workflow = workflow }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the step closes on attempt 3")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "review")
		Expect(attempts).To(HaveLen(3))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[2].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(attempts[2].GetAttempt()).To(Equal(int64(3)))
		Expect(closedSteps(detail)).To(HaveKey("review"))

		By("and no other step was superseded")
		for _, d := range detail.GetSteps() {
			Expect(d.GetStepRun().GetStatus()).
				NotTo(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED),
					"%s attempt %d", d.GetStepRun().GetStepName(), d.GetStepRun().GetAttempt())
		}
		Expect(stepRuns(detail, "implement")).To(HaveLen(1))

		// Each attempt after the first was given the last one's final
		// message: the retry carries feedback exactly as a revise does.
		prompts := adapter.reviewPrompts()
		Expect(prompts).To(HaveLen(3))
		Expect(prompts[0]).NotTo(ContainSubstring("the change still does the wrong thing"))
		Expect(prompts[1]).To(ContainSubstring("round 1: the change still does the wrong thing"))
		Expect(prompts[2]).To(ContainSubstring("round 2: the change still does the wrong thing"))
	})

	It("A check step with retries 1 and no on_failure runs twice before parking", func() {
		// clean-tree with nothing to fall back on: no revise target, one
		// retry, and a command that never passes.
		workflow := f.variant(ctx, "twice-checked", func(definition *millv1.Workflow) {
			check := stepOf(definition, "clean-tree")
			check.GetCheck().Command = []string{"false"}
			check.OnFailure = nil
			check.Retries = 1
		})
		f.script(f.quickFixScripts()...)

		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		detail := f.inspect(ctx)
		Expect(stepRuns(detail, "clean-tree")).To(HaveLen(2))
		// Nothing was sent back: the step had no on_failure to send it
		// anywhere, and the second failure is the breaker's.
		Expect(stepRuns(detail, "implement")).To(HaveLen(1))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(f.runOf(ctx).GetParkedReason()).To(Equal(millv1.ParkedReason_PARKED_REASON_RETRIES))
	})

	It("The breaker tells the whole story", func() {
		By("given a workflow whose clean-tree check fails because commit produced no commit")
		// The commit session says it cannot proceed and leaves the tree as
		// it found it; the check that follows is the one that fails, four
		// rounds and one revise away from the words that explain it.
		workflow := f.variant(ctx, "unclean-fix", func(definition *millv1.Workflow) {
			stepOf(definition, "clean-tree").GetCheck().Command =
				[]string{"sh", "-c", "echo 'the tree is dirty: README.md' >&2; exit 1"}
		})
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal(
				"Round 1: I cannot proceed. Committing this change requires approval.",
				nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("the change is made again", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal(
				"Round 2: I cannot proceed. Committing this change requires approval.",
				nil, usage(5)), Do: f.commits},
		)

		By("when the run parks")
		Expect(f.runner(func(o *run.Options) { o.Workflow = workflow }).
			Execute(ctx, f.nodeRef)).To(Succeed())
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		body := f.theGate(ctx).GetBody()

		By("then the gate body contains the commit session's final message from every round")
		Expect(body).To(ContainSubstring("Round 1: I cannot proceed."))
		Expect(body).To(ContainSubstring("Round 2: I cannot proceed."))

		By("and the clean-tree stderr from every round")
		rounds := len(stepRuns(f.inspect(ctx), "clean-tree"))
		Expect(rounds).To(BeNumerically(">", 1))
		Expect(strings.Count(body, "the tree is dirty: README.md")).
			To(BeNumerically(">=", rounds), "one for every attempt of the check")
	})
})

// retriedReview is quick-fix with a review step whose retries and on_failure
// the spec chooses: the step that fails on its own account, with or without
// anywhere to send the work.
func (f *fixture) retriedReview(ctx context.Context, model string, retries int32, onFailure *millv1.OnFailure) string {
	GinkgoHelper()
	return f.variant(ctx, "retried-review", func(definition *millv1.Workflow) {
		review := reviewStep(stepOf(definition, "implement").GetSkill())
		review.OnFailure = onFailure
		review.Retries = retries
		review.Harness = map[string]*millv1.HarnessSettings{
			run.DefaultHarness: {Options: options(map[string]any{"model": model})},
		}
		insertAfter(definition, "clean-tree", review)
	})
}
