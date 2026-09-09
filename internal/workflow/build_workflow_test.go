package workflow_test

import (
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/workflow"
)

// The `build-workflow` workflow as a definition: one read-only step that
// sizes a node and names the workflow that should deliver it
// (docs/domain-model.md §Selection).
//
// The step is the one a run with no project default executes before it has a
// workflow, so what it costs and what it may touch are part of the design and
// not a preference: read-only on both halves, a cheap model, and ten
// minutes.

var buildWorkflowDir = filepath.Join("..", "..", "workflows", "build-workflow")

var _ = Describe("The build-workflow workflow", Label("unit"), func() {
	var loaded *millv1.Workflow
	var skills []*millv1.Skill

	BeforeEach(func() {
		var err error
		loaded, skills, err = workflow.Load(buildWorkflowDir)
		Expect(err).NotTo(HaveOccurred())
	})

	It("is one skill step, `assess`, closing on structured evidence", func() {
		Expect(loaded.GetName()).To(Equal("build-workflow"))
		Expect(loaded.GetScope()).To(Equal(millv1.Scope_SCOPE_ORG), "every project may select it")
		Expect(loaded.GetSteps()).To(HaveLen(1))

		step := loaded.GetSteps()[0]
		Expect(step.GetName()).To(Equal("assess"))
		Expect(step.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_SKILL))
		Expect(step.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED))
		Expect(step.GetSession()).To(Equal("assess"))
		Expect(step.GetSkill().GetName()).To(Equal("build-workflow"))
		Expect(skills).To(HaveLen(1))
		Expect(skills[0].GetDigest()).To(Equal(step.GetSkill().GetDigest()))
	})

	It("is read-only, on a cheap model, and short", func() {
		step := loaded.GetSteps()[0]

		Expect(step.GetCapabilities()).To(Equal([]millv1.Capability{millv1.Capability_CAPABILITY_READ}))
		options := step.GetHarness()["claude"].GetOptions().AsMap()
		Expect(options).To(HaveKeyWithValue("model", "sonnet"))
		tools, _ := options["tools"].(map[string]any)
		Expect(tools).NotTo(HaveKey("deny"))
		Expect(tools["allow"]).To(ConsistOf("Glob", "Grep", "Read"),
			"a session with no write tool at all, and no shell")
		Expect(step.GetTimeout().AsDuration()).To(Equal(10 * time.Minute))
	})

	// The schema is the decision docs/domain-model.md §Selection records, in
	// the file the step names. It is loaded onto the step, so the workflow's
	// version changes when the schema does.
	It("names the assessment's schema, which requires a workflow, a tier, signals and reasoning", func() {
		schema := loaded.GetSteps()[0].GetOutputSchema()
		Expect(schema).NotTo(BeEmpty())

		Expect(evidence.ValidateJSON(schema, map[string]any{
			"workflow":  "quick-fix",
			"tier":      "quick-fix",
			"signals":   []any{"README.md is the only file the ask names"},
			"reasoning": "one line, no design question",
		})).To(Succeed())

		Expect(evidence.ValidateJSON(schema, map[string]any{
			"workflow": "quick-fix", "tier": "quick-fix", "signals": []any{}, "reasoning": "because",
		})).To(MatchError(ContainSubstring("/signals")), "a decision resting on nothing is not one")
		Expect(evidence.ValidateJSON(schema, map[string]any{
			"workflow": "quick-fix", "tier": "enormous", "signals": []any{"a"}, "reasoning": "because",
		})).To(MatchError(ContainSubstring("/tier")))
		Expect(evidence.ValidateJSON(schema, map[string]any{
			"tier": "quick-fix", "signals": []any{"a"}, "reasoning": "because",
		})).To(MatchError(ContainSubstring("workflow")))
	})
})
