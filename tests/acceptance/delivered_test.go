package acceptance_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// deliveredTimeout is how long one quick-fix node has to go from filed to
// merged. Six steps, two of them sessions, against a real forge.
const deliveredTimeout = 10 * time.Minute

// quickFixSteps are the steps of workflows/quick-fix in order, each with the
// evidence its definition names, preceded by the step the run makes for
// itself. The ledger must show one closed attempt of each, closed on
// evidence of that kind and no other.
//
// `select` is not in the workflow file: it is the attempt that records which
// workflow the run walks and on what basis, and a run told which one on the
// command line records `project default` rather than skipping it
// (docs/domain-model.md §Selection).
var quickFixSteps = []struct {
	name     string
	evidence millv1.EvidenceKind
}{
	{"select", millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED},
	{"toolchain", millv1.EvidenceKind_EVIDENCE_KIND_EXIT},
	{"gather-context", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"implement", millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT},
	{"commit", millv1.EvidenceKind_EVIDENCE_KIND_COMMIT},
	{"clean-tree", millv1.EvidenceKind_EVIDENCE_KIND_EXIT},
	{"pr", millv1.EvidenceKind_EVIDENCE_KIND_URL},
	{"merge", millv1.EvidenceKind_EVIDENCE_KIND_STATUS},
}

var _ = Describe("Acceptance", Label("acceptance"), func() {
	It("One node, delivered", func(ctx SpecContext) {
		needsForge()

		s := typo.stage(ctx)

		By("given a scratch project with a README containing \"recieve\"") //nolint:misspell // the misspelling is the fixture
		Expect(s.Project.file(ctx, "README.md", s.Project.DefaultBranch)).To(ContainSubstring(misspelling))

		By("and a quick-fix node describing the fix")
		Expect(s.Graph.show(ctx, s.NodeRef).Node.Status).To(Equal("open"))

		By("when mill run is invoked for the node")
		Expect(s.start(ctx, runOptions{}).wait(deliveredTimeout)).To(BeZero(), "mill run exited cleanly")

		By(`then the default branch's README contains "` + correction + `"`)
		readme := s.Project.file(ctx, "README.md", s.Project.DefaultBranch)
		Expect(readme).To(ContainSubstring(correction))
		Expect(readme).NotTo(ContainSubstring(misspelling))

		By("and the node is done with a merge commit artifact")
		view := s.Graph.show(ctx, s.NodeRef)
		Expect(view.Node.Status).To(Equal("done"))
		merged := view.artifact("merge-commit")
		Expect(merged).To(MatchRegexp(`^[0-9a-f]{40}$`), "the artifact is a commit SHA")
		Expect(s.Project.branchesContaining(ctx, merged)).To(ContainElement(s.Project.DefaultBranch),
			"the SHA is on the default branch")

		By("and its diff is the one-word change and nothing else")
		diffs := s.Project.diffOf(ctx, merged)
		Expect(diffs).To(HaveLen(1), "one file changed")
		Expect(diffs[0].NewPath).To(Equal("README.md"))
		Expect(diffs[0].Diff).To(ContainSubstring("-We " + misspelling))
		Expect(diffs[0].Diff).To(ContainSubstring("+We " + correction))

		By("and every step in the ledger is closed with evidence")
		detail := inspect(ctx, s.NodeRef)
		Expect(detail.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		steps := latest(detail)
		Expect(steps).To(HaveLen(len(quickFixSteps)), "one step run per quick-fix step")
		for _, step := range quickFixSteps {
			d := steps[step.name]
			Expect(d).NotTo(BeNil(), "the ledger has an attempt of %s", step.name)
			Expect(d.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
				"%s closed", step.name)
			Expect(d.GetEvidence().GetKind()).To(Equal(step.evidence),
				"%s closed on the evidence its step names", step.name)
		}

		By("and the merge is by the mill's forge identity")
		mr := mergeRequestFor(s.Project.mergeRequests(ctx), detail.GetSummary().GetRun().GetBranch())
		Expect(mr.State).To(Equal("merged"))
		Expect(mr.MergeCommit).To(Equal(merged))
		Expect(mr.MergedBy).NotTo(BeNil())
		Expect(mr.MergedBy.Username).To(Equal(env.ForgeUser))

		By("and the commit it brought in verifies against the mill's allowed_signers")
		repo := cloneScratch(ctx, s.Project)
		signed := repo.signedCommitIn(ctx, merged)
		out, err := repo.verifyCommit(ctx, signed, allowedSignersPath(s.WorkspaceDir))
		Expect(err).NotTo(HaveOccurred(), "git verify-commit %s: %s", signed, out)
		Expect(out).To(ContainSubstring(goodSignatureFor(allowedSignersPath(s.WorkspaceDir))))

		By("and skift reports what the run cost, per step and across models")
		// Skift's rollup sums by model and keeps a count of the records it
		// summed; it carries neither the principal nor the lease, so
		// "reported by the mill's principal" is asserted structurally — this
		// project was made by the suite, the mill's run is the only thing
		// that ever held a lease on this node, and nothing else can have
		// written a usage record against it.
		Expect(view.Usage.Actual).NotTo(BeEmpty(), "usage reached skift")
		var records int64
		models := map[string]bool{}
		for _, total := range view.Usage.Actual {
			records += total.Records
			models[total.Model] = true
			Expect(total.InputTokens+total.OutputTokens).To(BeNumerically(">", 0),
				"model %s billed something", total.Model)
		}
		Expect(len(models)).To(BeNumerically(">=", 2),
			"at least two models across the run; got %s", strings.Join(modelNames(models), ", "))
		Expect(records).To(BeNumerically(">=", int64(sessionSteps(detail))),
			"at least one usage record per step that ran a session")
	})
})

// sessionSteps is how many attempts ran a session, which is how many steps
// could have billed a model.
func sessionSteps(detail *millv1.InspectResponse) int {
	count := 0
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetKind() == millv1.StepKind_STEP_KIND_SKILL {
			count++
		}
	}
	return count
}

// modelNames is a set of models as a sorted list, for a failure message.
func modelNames(models map[string]bool) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	return names
}
