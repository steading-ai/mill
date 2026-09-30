package workflow_test

import (
	"path/filepath"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/workflow"
)

// The mill project's own workflows: the org definitions with the live tier
// as a check step, and nothing else changed (docs/sandbox.md §Self-hosting,
// step 3). They shadow `quick-fix` and `feature` for the project `mill`
// alone, which is the domain model's way of giving one project a step the
// org's definition does not have (docs/domain-model.md §Workflow, `scope`).
// The shadow is held to its original here: a step added to or changed in
// the org definition that the mill one did not follow is a drift this spec
// names, because nothing at run time would.

// liveTierStep is the step the mill definitions add.
const liveTierStep = "live-tier"

var _ = Describe("The mill project's workflows", Label("unit"), func() {
	DescribeTable("are the org definition with the live tier as a check step",
		func(name string) {
			org, orgSkills, err := workflow.Load(filepath.Join("..", "..", "workflows", name))
			Expect(err).NotTo(HaveOccurred())
			mill, millSkills, err := workflow.Load(filepath.Join("..", "..", "workflows", "mill-"+name))
			Expect(err).NotTo(HaveOccurred())

			By("shadowing the org definition for the mill project alone")
			Expect(mill.GetName()).To(Equal(name))
			Expect(mill.GetScope()).To(Equal(millv1.Scope_SCOPE_PROJECT))
			Expect(mill.GetProject()).To(Equal("mill"))

			By("carrying the live tier as a check step after clean-tree: the tier's own invocation, exit 0 as evidence, a failure revising implement")
			live := step(mill, liveTierStep)
			Expect(live).NotTo(BeNil(), "%s has no %s step", "mill-"+name, liveTierStep)
			Expect(live.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_CHECK))
			Expect(live.GetCheck().GetCommand()).To(Equal([]string{"sh", "-c", "MILL_SANDBOX_HOST=1 MILL_REQUIRE_FIXTURES=1 mise exec -- make test 1>&2"}))
			Expect(live.GetCheck().GetExit()).To(Equal([]int32{0}))
			Expect(live.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
			Expect(live.GetCapabilities()).To(Equal([]millv1.Capability{millv1.Capability_CAPABILITY_EXEC}))
			Expect(live.GetOnFailure().GetKind()).To(Equal(millv1.OnFailureKind_ON_FAILURE_KIND_REVISE))
			Expect(live.GetOnFailure().GetReviseTarget()).To(Equal("implement"))
			names := stepNames(mill)
			at := slices.Index(names, liveTierStep)
			Expect(names[at-1]).To(Equal("clean-tree"), "the tier runs on the committed tree, before the pull request")

			By("and being otherwise the org definition: the same steps, in order, pinning the same skills")
			Expect(slices.Delete(names, at, at+1)).To(Equal(stepNames(org)))
			for _, orgStep := range org.GetSteps() {
				millStep := step(mill, orgStep.GetName())
				Expect(proto.Equal(millStep, orgStep)).To(BeTrue(), "step %s differs from the org definition's:\n%v\n%v", orgStep.GetName(), millStep, orgStep)
			}
			Expect(millSkills).To(HaveLen(len(orgSkills)))
			for _, skill := range orgSkills {
				Expect(millSkills).To(ContainElement(WithTransform(func(s *millv1.Skill) string { return s.GetName() + "@" + s.GetDigest() },
					Equal(skill.GetName()+"@"+skill.GetDigest()))))
			}
		},
		Entry("quick-fix", "quick-fix"),
		Entry("feature", "feature"),
	)
})

// stepNames is a definition's steps, in order.
func stepNames(w *millv1.Workflow) []string {
	var names []string
	for _, s := range w.GetSteps() {
		names = append(names, s.GetName())
	}
	return names
}
