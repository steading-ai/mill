package workflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/workflow"
)

// The `feature` workflow as a definition: the tier that plans before it
// writes, reviews the plan, and reviews the change (docs/prd.md §8 O1,
// docs/domain-model.md §Workflow).
//
// Two things here are the loader's rather than the definition's, and are
// specified as such: a step may reference a skill another workflow's
// directory carries, and the record it loads is the same record — same name,
// same digest — the owning workflow pins.

var featureDir = filepath.Join("..", "..", "workflows", "feature")

var _ = Describe("The feature workflow", Label("unit"), func() {
	var loaded *millv1.Workflow
	var skills []*millv1.Skill

	BeforeEach(func() {
		var err error
		loaded, skills, err = workflow.Load(featureDir)
		Expect(err).NotTo(HaveOccurred())
	})

	It("loads with the toolchain step and twelve steps, in order", func() {
		names := []string{}
		for _, s := range loaded.GetSteps() {
			names = append(names, s.GetName())
		}
		Expect(names).To(Equal([]string{
			"toolchain", "gather-context", "think", "plan", "review-plan", "plan-approval",
			"implement", "simplify", "commit", "clean-tree", "pr", "review", "merge",
		}))
	})

	It("every skill step names a model, an effort, capabilities, retries and a timeout", func() {
		for _, s := range loaded.GetSteps() {
			if s.GetKind() != millv1.StepKind_STEP_KIND_SKILL {
				continue
			}
			options := s.GetHarness()["claude"].GetOptions().AsMap()
			Expect(options).To(HaveKey("model"), s.GetName())
			Expect(options).To(HaveKey("effort"), s.GetName())
			Expect(s.GetCapabilities()).NotTo(BeEmpty(), s.GetName())
			Expect(s.GetRetries()).To(BeNumerically(">", 0), s.GetName())
			Expect(s.GetTimeout().AsDuration()).To(BeNumerically(">", 0), s.GetName())
			Expect(s.GetSession()).NotTo(BeEmpty(), s.GetName())
			Expect(s.GetSkill().GetDigest()).NotTo(BeEmpty(), s.GetName())
		}
	})

	It("implement is three hours and two retries, and no step is longer", func() {
		implement := step(loaded, "implement")
		Expect(implement.GetTimeout().AsDuration().Hours()).To(BeNumerically("==", 3))
		Expect(implement.GetRetries()).To(BeNumerically("==", 2))
		for _, s := range loaded.GetSteps() {
			if s.GetKind() != millv1.StepKind_STEP_KIND_SKILL {
				continue
			}
			Expect(s.GetTimeout().AsDuration()).To(BeNumerically("<=", implement.GetTimeout().AsDuration()),
				"%s outlasts implement", s.GetName())
		}
	})

	It("plan closes on a file in the checkout, named for the run", func() {
		plan := step(loaded, "plan")
		Expect(plan.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT))
		Expect(plan.GetArtifact().GetPath()).To(Equal("docs/plans/{{.Date}}-{{.NodeSlug}}.md"))
		Expect(plan.GetCapabilities()).To(ContainElement(millv1.Capability_CAPABILITY_WRITE))
	})

	It("review-plan closes on a verdict and sends a revise back to plan", func() {
		s := step(loaded, "review-plan")
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_VERDICT))
		Expect(s.GetOnFailure().GetKind()).To(Equal(millv1.OnFailureKind_ON_FAILURE_KIND_REVISE))
		Expect(s.GetOnFailure().GetReviseTarget()).To(Equal("plan"))
	})

	It("plan-approval auto-approves an approved plan at this tier or below, and asks otherwise", func() {
		s := step(loaded, "plan-approval")
		Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_GATE))
		Expect(s.GetGate().GetAuto()).To(Equal(
			"verdict('review-plan').decision == approve && tier <= feature"))
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_GATE))

		// The ask has to be answerable where it is read: it names the node
		// and quotes the plan, so a person carrying it to Slack carries the
		// plan with it (docs/prd.md R-21).
		Expect(s.GetGate().GetAsk().GetTitle()).To(ContainSubstring("{{.NodeRef}}"))
		body := s.GetGate().GetAsk().GetBody()
		Expect(body).To(ContainSubstring("{{.NodeRef}}"))
		Expect(body).To(ContainSubstring(`{{head 40 (artifact "plan")}}`))
		Expect(s.GetGate().GetDenyResult()).To(ContainSubstring("rejected"))
		Expect(s.GetGate().GetDenyResult()).To(ContainSubstring("the run stopped"))
	})

	It("merge auto-approves on an approving verdict at this tier or below", func() {
		s := step(loaded, "merge")
		Expect(s.GetGate().GetAuto()).To(Equal("review.decision == approve && tier <= feature"))
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_STATUS))
	})

	It("its check script is in the directory the step names", func() {
		info, err := os.Stat(filepath.Join(featureDir, "scripts", "clean-tree"))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()&0o111).NotTo(BeZero(), "a check is executed, so it has to be executable")
	})

	It("no skill waits for a human", func() {
		for _, skill := range skills {
			for _, forbidden := range []string{"AskUserQuestion", "ExitPlanMode", "ask the user", "wait for confirmation"} {
				Expect(skill.GetBody()).NotTo(ContainSubstring(forbidden),
					"skill %s was adapted for unattended use", skill.GetName())
			}
		}
	})

	// The scenario. docs/domain-model.md §Step: a reviewer declares both
	// halves — no write capability, and a tool policy that keeps the write
	// tools out of the session at all. Bash stays: on the machine backend
	// the checkout is mounted read-only for a step without write
	// (docs/sandbox.md §Exec), and the shell is how the reviewer fetches
	// the diff and runs the suite.
	Describe("The reviewer cannot write", func() {
		It("The reviewer cannot write", func() {
			By("given the feature workflow is loaded")
			review := step(loaded, "review")

			By("when the review step's tool policy is read")
			options := review.GetHarness()["claude"].GetOptions().AsMap()
			tools, ok := options["tools"].(map[string]any)
			Expect(ok).To(BeTrue(), "the review step names a tool policy")

			By("then the write tools are denied, and the shell is not")
			Expect(tools["deny"]).To(ConsistOf("Write", "Edit", "NotebookEdit"))
			Expect(tools).NotTo(HaveKey("allow"), "an allow list beside a deny list is two answers to one question")

			By("and the step's capabilities are read only")
			Expect(review.GetCapabilities()).To(Equal([]millv1.Capability{millv1.Capability_CAPABILITY_READ}))
			Expect(review.GetSession()).To(Equal("review"))
			Expect(review.GetSession()).NotTo(Equal(step(loaded, "implement").GetSession()))
		})
	})

	// The reviewer is asked for exactly what the verifier accepts: the
	// schema lives in a file beside the skill, the step names that file, and
	// the loader refuses any schema but the mill's own
	// (docs/domain-model.md §Verdict).
	Describe("the review step's output schema", func() {
		It("is the mill's verdict schema, loaded from the file the step names", func() {
			declared := step(loaded, "review").GetOutputSchema()
			Expect(declared).NotTo(BeEmpty())

			file, err := os.ReadFile(filepath.Join(featureDir, "skills", "review", "verdict.schema.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(declared).To(Equal(string(file)), "the step carries the file's content, not its path")

			var fromFile, mills any
			Expect(json.Unmarshal(file, &fromFile)).To(Succeed())
			Expect(json.Unmarshal([]byte(evidence.VerdictSchema), &mills)).To(Succeed())
			Expect(fromFile).To(Equal(mills), "the file and the verifier cannot disagree")
		})

		It("refuses a verdict step asking for any other shape", func() {
			dir := GinkgoT().TempDir()
			Expect(workflow.Write(dir, loaded, skills)).To(Succeed())
			body, err := os.ReadFile(filepath.Join(dir, "workflow.yaml"))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "workflow.yaml"),
				[]byte(strings.ReplaceAll(string(body), `"decision"`, `"verdict"`)), 0o600)).To(Succeed())

			_, _, err = workflow.Load(dir)
			Expect(err).To(MatchError(ContainSubstring("output_schema is not the verdict schema")))
		})
	})

	Describe("a skill another workflow's directory carries", func() {
		It("loads as the same record its own workflow pins", func() {
			quick, quickSkills, err := workflow.Load(filepath.Join("..", "..", "workflows", "quick-fix"))
			Expect(err).NotTo(HaveOccurred())

			digests := map[string]string{}
			for _, skill := range quickSkills {
				digests[skill.GetName()] = skill.GetDigest()
			}
			for _, name := range []string{"gather-context", "implement", "commit", "pr"} {
				borrowed := step(loaded, name).GetSkill()
				Expect(borrowed.GetName()).To(Equal(name), "the step pins the skill's own name, not the reference")
				Expect(borrowed.GetDigest()).To(Equal(digests[name]),
					"%s is one record, whichever workflow reached for it", name)
				Expect(borrowed.GetDigest()).To(Equal(step(quick, name).GetSkill().GetDigest()))
			}
		})

		It("refuses a reference that climbs out of the workflows directory", func() {
			_, _, err := workflow.Load(dir(`
name: reaching
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: ../../etc/implement
    evidence: artifact
    capabilities: [read]
`, nil))
			Expect(err).To(MatchError(ContainSubstring("a sibling workflow's name and a name")))
		})
	})
})
