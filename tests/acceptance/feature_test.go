package acceptance_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
)

// featureTimeout is how long one feature node has to reach its pull request.
// Nine sessions rather than quick-fix's four, two of them a reviewer, so it
// is three times the quick-fix budget and no more: a step that needs longer
// is a step whose plan was wrong.
const featureTimeout = 30 * time.Minute

// featureSteps are the steps of workflows/feature up to and including `pr`,
// each with the evidence its definition names. Everything after `pr` — the
// reviewer and the merge gate — is the `review` node's to prove.
var featureSteps = []struct {
	name     string
	evidence millv1.EvidenceKind
}{
	{"gather-context", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"think", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"plan", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"review-plan", millv1.EvidenceKind_EVIDENCE_KIND_VERDICT},
	{"plan-approval", millv1.EvidenceKind_EVIDENCE_KIND_GATE},
	{"implement", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"simplify", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"commit", millv1.EvidenceKind_EVIDENCE_KIND_COMMIT},
	{"clean-tree", millv1.EvidenceKind_EVIDENCE_KIND_EXIT},
	{"pr", millv1.EvidenceKind_EVIDENCE_KIND_URL},
}

// featureFixture is the typo node again, filed for the tier that plans first.
// The change is deliberately trivial: what is under test is the workflow, not
// the code, and a larger change would only buy a longer session.
var featureFixture = fixture{
	Name:  "feature",
	Files: typo.Files,
	Title: typo.Title,
	Body:  typo.Body,
}

var _ = Describe("Acceptance", Label("acceptance"), func() {
	It("One node, planned and delivered", func(ctx SpecContext) {
		s := featureFixture.stage(ctx)

		By("given a feature node on a scratch project with one file to change")
		Expect(s.Project.file(ctx, "README.md", s.Project.DefaultBranch)).To(ContainSubstring(misspelling)) //nolint:misspell // the misspelling is the fixture

		By("when mill run is invoked for it with the feature workflow")
		Expect(s.start(ctx, runOptions{Workflow: "feature", Tier: "feature"}).wait(featureTimeout)).
			To(BeZero(), "mill run exited cleanly")

		By("then every step through pr is closed on the evidence its definition names")
		detail := inspect(ctx, s.NodeRef)
		steps := latest(detail)
		for _, step := range featureSteps {
			d := steps[step.name]
			Expect(d).NotTo(BeNil(), "the ledger has an attempt of %s", step.name)
			Expect(d.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
				"%s closed", step.name)
			Expect(d.GetEvidence().GetKind()).To(Equal(step.evidence),
				"%s closed on the evidence its step names", step.name)
		}

		By("and the plan is an artifact dagger holds for the run")
		// `artifact` evidence only closes when dagger confirms it holds an
		// artifact of that digest, so a closed plan step is the attachment.
		Expect(steps["plan"].GetEvidence().GetArtifact()).NotTo(BeEmpty())

		By("and the plan is in the branch, under docs/plans, naming the run")
		branch := detail.GetSummary().GetRun().GetBranch()
		plans := s.Project.tree(ctx, "docs/plans", branch)
		Expect(plans).To(HaveLen(1), "one plan, named for the run's node and date")
		Expect(s.Project.file(ctx, plans[0], branch)).To(SatisfyAll(
			ContainSubstring("run: "+detail.GetSummary().GetRun().GetId()),
			ContainSubstring("node: "+s.NodeRef)))

		By("and the reviewer ran in a session of its own")
		Expect(steps["review-plan"].GetStepRun().GetSessionId()).NotTo(BeEmpty())
		Expect(steps["review-plan"].GetStepRun().GetSessionId()).
			NotTo(Equal(steps["implement"].GetStepRun().GetSessionId()))

		By("and the pull request the run opened is for its branch")
		mr := mergeRequestFor(s.Project.mergeRequests(ctx), branch)
		Expect(mr.SourceBranch).To(Equal(branch))
		Expect(strings.TrimSpace(steps["pr"].GetEvidence().GetUrl())).NotTo(BeEmpty())
	})
})
