package run_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// The reviewer's half of `feature`: what a verdict does to the run.
//
// A verdict is the one thing a session produces that the mill acts on, and
// it acts on it three ways — a `revise` sends the work back with the
// findings as the next prompt, output that is not a verdict fails the step
// without sending anybody anything, and either way the verdict goes on the
// node where a person reads it (docs/domain-model.md §Verdict, R-23–R-26).

// revises is a reviewer's structured output when it says no: a decision and
// the finding that blocks it, tied to the criterion it is against.
func revises(criterion, summary string) map[string]any {
	return map[string]any{
		"decision": "revise",
		"findings": []any{map[string]any{
			"criterion": criterion,
			"severity":  "blocking",
			"summary":   summary,
			"location":  "internal/run/review.go:1",
		}},
	}
}

var _ = Describe("The reviewer's verdict", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.pushWorkflowFrom(ctx, featureDir)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	// writesPlan is the plan step's session: the mill reads the file back
	// as that step's artifact, so a run that never wrote one stops there.
	writesPlan := func(harness.Session) {
		defer GinkgoRecover()
		Expect(f.workspace.Write(context.Background(), planPath(f.nodeRef),
			[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)))).To(Succeed())
	}

	// pass is one clean run of `feature` up to the review step: the eight
	// skill turns before it, in the workflow's order. Every turn that could
	// be the commit step commits, because that step closes on a commit that
	// was not on the branch when it started and on nothing else.
	pass := func() []fake.Script {
		return []fake.Script{
			{Terminal: fake.Terminal("context gathered", nil, usage(10))},
			{Terminal: fake.Terminal("explored the problem", nil, usage(10))},
			{Terminal: fake.Terminal("planned", nil, usage(10)), Do: writesPlan},
			{Terminal: fake.Terminal("", approves("AC-1", "the plan names its proof"), usage(5))},
			{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			{Terminal: fake.Terminal("simplified", nil, usage(10)), Do: f.commits},
			{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
		}
	}

	// featureRun drives the whole workflow with the reviewer's turns
	// scripted by the caller: the first is the answer to the first review,
	// and the last is replayed for every review after it.
	featureRun := func(reviews ...fake.Script) {
		GinkgoHelper()
		scripts := pass()
		for i, review := range reviews {
			scripts = append(scripts, review)
			if i+1 < len(reviews) {
				// The implementer's next pass, which a revise sends it
				// back to: implement, simplify, commit, pr again.
				scripts = append(scripts, pass()[4:]...)
			}
		}
		f.script(scripts...)
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
		}).Execute(ctx, f.nodeRef)).To(Succeed())
	}

	It("A reviewer's no goes back to the implementer", func() {
		By("given a feature run at the review step")
		const finding = "the second criterion has no spec: nothing fails if the step is skipped"

		By("when the reviewer's verdict is revise with a blocking finding on criterion 2")
		featureRun(
			fake.Script{Terminal: fake.Terminal("", revises("AC-2", finding), usage(5))},
			fake.Script{Terminal: fake.Terminal("", approves("AC-2", "the spec asserts the Then clause now"), usage(5))},
		)

		By("then implement, simplify, commit, clean-tree and pr are superseded")
		detail := f.inspect(ctx)
		for _, step := range []string{"implement", "simplify", "commit", "clean-tree", "pr"} {
			Expect(stepRuns(detail, step)[0].GetStatus()).
				To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED), "%s", step)
			Expect(stepRuns(detail, step)).To(HaveLen(2), "%s ran again", step)
		}
		Expect(after(detail, "review")).To(Equal("implement"))

		By("and the implementer's next prompt contains the finding and names criterion 2")
		calls := f.harness.Calls()
		Expect(calls).To(HaveLen(14))
		next := calls[9].Prompt.Text
		Expect(next).To(ContainSubstring(finding))
		Expect(next).To(ContainSubstring("AC-2"))
		Expect(next).To(ContainSubstring("blocking"))

		By("and the reviewer is told where the change is, not handed the diff")
		first := calls[8].Prompt.Text
		Expect(first).To(ContainSubstring("## The change you are reviewing"))
		Expect(first).To(ContainSubstring("on branch " + f.runOf(ctx).GetBranch()))
		base := stepRuns(detail, "implement")[0].GetBaseCommit()
		Expect(base).NotTo(BeEmpty())
		Expect(first).To(ContainSubstring("Run `git diff "+base[:7]), "the base is where implement's first attempt started")
		Expect(first).To(ContainSubstring("session-1.txt"), "the files the run changed are named")
		Expect(first).NotTo(ContainSubstring("```diff"), "the diff itself is the reviewer's to fetch")

		By("and the reviewer's second prompt says implement ran again in answer to its findings, and names no file")
		Expect(calls[8].Prompt.Text).NotTo(ContainSubstring("## What changed since your last verdict"),
			"the first review is not a second look")
		second := calls[13].Prompt.Text
		Expect(second).To(ContainSubstring("## What changed since your last verdict"))
		Expect(second).To(ContainSubstring("The `implement` step ran again (attempt 2) in answer to your findings"))
		Expect(second).NotTo(ContainSubstring("The file you are judging"))

		// And the run went on: the second verdict approved.
		Expect(closedSteps(detail)).To(HaveKeyWithValue("review", millv1.EvidenceKind_EVIDENCE_KIND_VERDICT))
		Expect(evidenceOf(detail, "review").GetVerdict().GetDecision()).
			To(Equal(millv1.VerdictDecision_VERDICT_DECISION_APPROVE))
	})

	It("A verdict is parsed, not read", func() {
		By("given a reviewer session whose final message praises the work in prose and emits no structured output")
		featureRun(fake.Script{
			Terminal: fake.Terminal("Beautiful work. Every criterion is met and the specs are a pleasure to read. Approved.",
				nil, usage(5)),
		})

		By("when the step's evidence is verified")
		detail := f.inspect(ctx)

		By("then the step run is failed as malformed")
		for _, sr := range stepRuns(detail, "review") {
			Expect(sr.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		}
		Expect(closedSteps(detail)).NotTo(HaveKey("review"))
		// It is not a revise: nothing was judged, so the implementer's work
		// is not sent back and its attempt is not spent.
		Expect(stepRuns(detail, "implement")).To(HaveLen(1))
		Expect(stepRuns(detail, "implement")[0].GetStatus()).
			To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))

		By("and the message names the missing decision field")
		comments := f.comments(ctx)
		Expect(comments).To(HaveLen(1))
		Expect(comments[0]).To(ContainSubstring("decision"))
		Expect(comments[0]).NotTo(ContainSubstring("Beautiful work"),
			"the session's prose is not read, and is not what a human is shown")
	})

	It("The verdict is on the record", func() {
		By("given a feature run whose review approved")
		featureRun(fake.Script{
			Terminal: fake.Terminal("the change is good",
				approves("AC-1", "the change matches the plan"), usage(5)),
		})

		By("when the node is shown in skift")
		verdict := artifactNamed(f.artifactsOn(ctx), "review-1.1.md")

		By("then a text artifact named for the step, the generation and the attempt carries the verdict with its findings and the reviewing session's id")
		Expect(verdict).To(ContainSubstring("decision: approve"))
		Expect(verdict).To(ContainSubstring("the change matches the plan"))
		Expect(verdict).To(ContainSubstring("AC-1"))
		Expect(verdict).To(ContainSubstring(run.SessionID(f.runOf(ctx).GetContextId(), 1, "review")))
		Expect(verdict).NotTo(ContainSubstring("the change is good"),
			"the verdict is what the mill parsed, not what the session said")
	})

	// The reviewer's second look, after its own revise sent the work back.
	// Its session is resumed, so the prompt it gets is the one it already
	// answered plus what the mill has to say about the revision
	// (docs/domain-model.md §Revise, "What the reviewer is told on its way
	// back"). A run once spent three review rounds on one finding because
	// the second and third prompts said nothing new and the reviewer
	// answered them from memory (mill#90).
	Describe("A second look", func() {
		// writesAnotherPlan is a plan session that revised: different bytes
		// from writesPlan's.
		writesAnotherPlan := func(harness.Session) {
			defer GinkgoRecover()
			Expect(f.workspace.Write(context.Background(), planPath(f.nodeRef),
				[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)+"\n2. Revised.\n"))).To(Succeed())
		}

		// planRevised is a feature run whose first review-plan verdict is
		// revise, whose plan step then runs again as replan, and whose
		// second review-plan approves; then the rest of the workflow, with
		// the review approving. Eleven calls: the second review-plan is the
		// sixth.
		planRevised := func(replan func(harness.Session)) []fake.Call {
			GinkgoHelper()
			scripts := pass()[:3]
			scripts = append(scripts,
				fake.Script{Terminal: fake.Terminal("", revises("AC-1", "the plan names no proof"), usage(5))},
				fake.Script{Terminal: fake.Terminal("planned again", nil, usage(10)), Do: replan},
				fake.Script{Terminal: fake.Terminal("", approves("AC-1", "the plan names its proof"), usage(5))},
			)
			scripts = append(scripts, pass()[4:]...)
			scripts = append(scripts, fake.Script{Terminal: fake.Terminal("", approves("AC-1", "the change matches the plan"), usage(5))})
			f.script(scripts...)
			Expect(f.runner(func(o *run.Options) {
				o.Workflow = "feature"
				o.WorkflowDir = mustAbs(featureDir)
			}).Execute(ctx, f.nodeRef)).To(Succeed())
			calls := f.harness.Calls()
			Expect(calls).To(HaveLen(11))
			return calls
		}

		It("A reviewer's second look is told the plan was revised", func() {
			By("given a feature run whose review-plan verdict was revise")
			By("and the plan step ran again and wrote a different plan")
			calls := planRevised(writesAnotherPlan)

			By("when review-plan runs again")
			second := calls[5].Prompt.Text

			By("then its prompt says the plan step ran again in answer to its findings")
			Expect(second).To(ContainSubstring("## What changed since your last verdict"))
			Expect(second).To(ContainSubstring("The `plan` step ran again (attempt 2) in answer to your findings"))

			By("and names the plan's path")
			Expect(second).To(ContainSubstring(planPath(f.nodeRef)))

			By("and says the file differs from the one it judged last")
			Expect(second).To(ContainSubstring("differs from the one you judged last"))
			Expect(second).NotTo(ContainSubstring("byte-identical"))
		})

		It("A plan the planner did not change is named as unchanged", func() {
			By("given a feature run whose review-plan verdict was revise")
			By("and the plan step ran again and wrote the same bytes")
			calls := planRevised(writesPlan)

			By("when review-plan runs again")
			second := calls[5].Prompt.Text

			By("then its prompt says the file is byte-identical to the one it judged last")
			Expect(second).To(ContainSubstring("## What changed since your last verdict"))
			Expect(second).To(ContainSubstring("byte-identical to the one you judged last"))
			Expect(second).NotTo(ContainSubstring("differs"))
		})

		It("The first review of a plan is told where the plan is", func() {
			By("given a feature run at its first review-plan attempt")
			calls := planRevised(writesAnotherPlan)

			By("when review-plan runs")
			first := calls[3].Prompt.Text

			By("then its prompt names the plan's path under \"The file you are judging\"")
			Expect(first).To(ContainSubstring("## The file you are judging"))
			Expect(first).To(ContainSubstring("`plan` step wrote `" + planPath(f.nodeRef) + "`"))

			By("and carries no \"What changed since your last verdict\" heading")
			Expect(first).NotTo(ContainSubstring("## What changed since your last verdict"))
		})
	})
})

// artifactNamed is the text of the artifact a node carries under a name.
func artifactNamed(held []*skiftv1.Artifact, name string) string {
	GinkgoHelper()
	for _, artifact := range held {
		if artifact.GetName() != name {
			continue
		}
		var b strings.Builder
		for _, part := range artifact.GetParts() {
			b.WriteString(part.GetText().GetText())
		}
		return b.String()
	}
	Fail("no artifact named " + name)
	return ""
}
