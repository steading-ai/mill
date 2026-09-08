package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/evidence"
	"github.com/ivy/mill/internal/workflow"
)

// skillFile is a minimal SKILL.md: the two fields the spec requires and a
// body.
func skillFile(name, body string) string {
	return "---\nname: " + name + "\ndescription: The " + name + " step.\n---\n\n" + body + "\n"
}

// dir writes a workflow directory from a definition and a set of skill
// bodies keyed by name, and returns its path.
func dir(definition string, skills map[string]string) string {
	GinkgoHelper()
	root := GinkgoT().TempDir()
	Expect(os.WriteFile(filepath.Join(root, workflow.DefinitionFile), []byte(definition), 0o644)).To(Succeed())
	for name, body := range skills {
		skill := filepath.Join(root, workflow.SkillsDir, name)
		Expect(os.MkdirAll(skill, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(skill, workflow.SkillFile), []byte(skillFile(name, body)), 0o644)).To(Succeed())
	}
	return root
}

// step finds a step by name, failing the spec when the workflow has none.
func step(w *millv1.Workflow, name string) *millv1.Step {
	GinkgoHelper()
	for _, s := range w.GetSteps() {
		if s.GetName() == name {
			return s
		}
	}
	Fail("no step named " + name)
	return nil
}

var _ = Describe("Loading a workflow directory", Label("unit"), func() {
	It("reads the definition and the skills its steps pin", func() {
		loaded, skills, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    capabilities: [read, write]
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.GetName()).To(Equal("tiny"))
		Expect(loaded.GetScope()).To(Equal(millv1.Scope_SCOPE_ORG), "a definition that names no scope is the org's")
		Expect(loaded.GetVersion()).NotTo(BeEmpty())
		Expect(skills).To(HaveLen(1))
		Expect(skills[0].GetBody()).To(Equal("Make the change."))
		Expect(step(loaded, "implement").GetSkill().GetDigest()).To(Equal(skills[0].GetDigest()),
			"a step pins the skill it ran by digest")
		Expect(step(loaded, "implement").GetRetries()).To(BeNumerically("==", 3), "the documented default")
	})

	It("refuses a key it does not understand", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    models: opus
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("models")))
	})

	It("fills the documented defaults for how a status step watches the forge's checks", func() {
		loaded, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.GetStatus().GetInterval().AsDuration()).To(Equal(evidence.DefaultStatusInterval))
		Expect(loaded.GetStatus().GetTimeout().AsDuration()).To(Equal(evidence.DefaultStatusTimeout))
	})

	It("reads the watch interval and cap a workflow names", func() {
		loaded, _, err := workflow.Load(dir(`
name: tiny
status:
  interval: 15s
  timeout: 30m
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.GetStatus().GetInterval().AsDuration()).To(Equal(15 * time.Second))
		Expect(loaded.GetStatus().GetTimeout().AsDuration()).To(Equal(30 * time.Minute))
	})

	It("refuses a watch interval that is not a duration", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
status:
  interval: soon
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("status: interval")))
	})

	It("refuses a step whose skill directory is not there", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: nonesuch
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("nonesuch")))
	})

	It("refuses two steps with the same name", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
  - name: implement
    kind: check
    check: {command: [scripts/check]}
    evidence: exit
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("the name is already taken")))
	})

	It("refuses a revise whose target is a later step, naming both steps", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: check
    kind: check
    check: {command: [scripts/check]}
    evidence: exit
    on_failure: revise implement
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("step check")))
		Expect(err).To(MatchError(ContainSubstring("revise targets implement")))
		Expect(err).To(MatchError(ContainSubstring("not earlier")))
	})

	It("refuses a revise whose target is the failing step itself", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    on_failure: revise implement
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("not earlier")))
	})

	It("refuses a gate after a write step with no url evidence between them, naming the gate", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    capabilities: [write]
  - name: merge
    kind: gate
    gate:
      ask:
        title: Merge the change
    evidence: gate
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("step merge")))
		Expect(err).To(MatchError(ContainSubstring("url")))
	})

	It("accepts a gate after a write step once a url step has pushed the branch", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    capabilities: [write]
  - name: pr
    kind: skill
    session: implement
    skill: pr
    evidence: url
    capabilities: [forge]
  - name: merge
    kind: gate
    gate:
      ask:
        title: Merge the change
    evidence: gate
`, map[string]string{"implement": "Make the change.", "pr": "Open the PR."}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("accepts a gate before anything has written", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: plan
    kind: skill
    session: implement
    skill: implement
    evidence: structured
    capabilities: [read]
  - name: approve-plan
    kind: gate
    gate:
      ask:
        title: Approve the plan
    evidence: gate
`, map[string]string{"implement": "Plan the change."}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a step with no evidence", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("evidence is required")))
	})

	It("refuses a skill whose frontmatter carries a field the spec does not allow", func() {
		root := dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, nil)
		skill := filepath.Join(root, workflow.SkillsDir, "implement")
		Expect(os.MkdirAll(skill, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(skill, workflow.SkillFile),
			[]byte("---\nname: implement\ndescription: d\nmodel: opus\n---\n\nBody.\n"), 0o644)).To(Succeed())

		_, _, err := workflow.Load(root)
		Expect(err).To(MatchError(ContainSubstring("unexpected fields in frontmatter: model")))
	})
})

var _ = Describe("Hashing a workflow", Label("unit"), func() {
	definition := `
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`

	It("is the same for the same content and different for one changed byte", func() {
		first, firstSkills, err := workflow.Load(dir(definition, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		same, _, err := workflow.Load(dir(definition, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(same.GetVersion()).To(Equal(first.GetVersion()), "the same files are the same version")

		changed, changedSkills, err := workflow.Load(dir(definition, map[string]string{"implement": "Make the change!"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(changedSkills[0].GetDigest()).NotTo(Equal(firstSkills[0].GetDigest()),
			"one byte of a skill body is a new skill digest")
		Expect(changed.GetVersion()).NotTo(Equal(first.GetVersion()),
			"and a new workflow version, because a step pins its skill by digest")
	})

	It("ignores where a version came from and when it was stored", func() {
		loaded, _, err := workflow.Load(dir(definition, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		version, err := workflow.Version(loaded)
		Expect(err).NotTo(HaveOccurred())

		loaded.Source = &millv1.Source{Kind: millv1.SourceKind_SOURCE_KIND_CLI, PushedBy: "someone-else"}
		loaded.Id = "01JZZZZZZZZZZZZZZZZZZZZZZZ"
		again, err := workflow.Version(loaded)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(version))
	})
})

var _ = Describe("The quick-fix workflow", Label("unit"), func() {
	var loaded *millv1.Workflow
	var skills []*millv1.Skill

	BeforeEach(func() {
		var err error
		loaded, skills, err = workflow.Load(filepath.Join("..", "..", "workflows", "quick-fix"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("loads with the six steps of the Quick Fix tier, in order", func() {
		names := []string{}
		for _, s := range loaded.GetSteps() {
			names = append(names, s.GetName())
		}
		Expect(names).To(Equal([]string{"gather-context", "implement", "commit", "clean-tree", "pr", "merge"}))
	})

	DescribeTable("a skill step runs in a session with a capability set and an evidence kind",
		func(name, session string, capabilities []millv1.Capability, evidence millv1.EvidenceKind) {
			s := step(loaded, name)
			Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_SKILL))
			Expect(s.GetSession()).To(Equal(session))
			Expect(s.GetSkill().GetName()).To(Equal(name))
			Expect(s.GetSkill().GetDigest()).NotTo(BeEmpty())
			Expect(s.GetCapabilities()).To(Equal(capabilities))
			Expect(s.GetEvidence()).To(Equal(evidence))
		},
		Entry("gather-context reads", "gather-context", "implement",
			[]millv1.Capability{millv1.Capability_CAPABILITY_READ}, millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT),
		Entry("implement reads, writes, and runs commands", "implement", "implement",
			[]millv1.Capability{
				millv1.Capability_CAPABILITY_READ,
				millv1.Capability_CAPABILITY_WRITE,
				millv1.Capability_CAPABILITY_EXEC,
			}, millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT),
		Entry("commit writes and closes on a commit", "commit", "implement",
			[]millv1.Capability{millv1.Capability_CAPABILITY_WRITE}, millv1.EvidenceKind_EVIDENCE_KIND_COMMIT),
		Entry("pr reaches the forge and closes on a URL", "pr", "implement",
			[]millv1.Capability{millv1.Capability_CAPABILITY_FORGE}, millv1.EvidenceKind_EVIDENCE_KIND_URL),
	)

	It("clean-tree is a check whose failure revises implement", func() {
		s := step(loaded, "clean-tree")
		Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_CHECK))
		Expect(s.GetCheck().GetCommand()).To(Equal([]string{"scripts/clean-tree"}))
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
		Expect(s.GetOnFailure().GetKind()).To(Equal(millv1.OnFailureKind_ON_FAILURE_KIND_REVISE))
		Expect(s.GetOnFailure().GetReviseTarget()).To(Equal("implement"))
	})

	It("merge is a gate that auto-approves and closes on a status", func() {
		s := step(loaded, "merge")
		Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_GATE))
		Expect(s.GetGate().GetAuto()).To(Equal("true"), "repo protections are the repo's job")
		Expect(s.GetGate().GetAsk().GetTitle()).NotTo(BeEmpty())
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_STATUS))
	})

	It("its check script is in the directory the step names", func() {
		info, err := os.Stat(filepath.Join("..", "..", "workflows", "quick-fix", "scripts", "clean-tree"))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()&0o111).NotTo(BeZero(), "a check is executed, so it has to be executable")
	})

	It("every skill declares only the six fields the agent-skills spec allows", func() {
		Expect(skills).To(HaveLen(4))
		allowed := []string{"allowed-tools", "compatibility", "description", "license", "metadata", "name"}
		for _, skill := range skills {
			path := filepath.Join("..", "..", "workflows", "quick-fix", workflow.SkillsDir, skill.GetName(), workflow.SkillFile)
			content, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())

			text := string(content)
			Expect(text).To(HavePrefix("---\n"))
			head := text[len("---\n") : len("---\n")+strings.Index(text[len("---\n"):], "\n---")]
			for _, line := range strings.Split(head, "\n") {
				if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-") {
					continue
				}
				Expect(allowed).To(ContainElement(strings.SplitN(line, ":", 2)[0]),
					"%s declares a field outside the spec", path)
			}
			Expect(skill.GetName()).NotTo(BeEmpty())
			Expect(skill.GetDescription()).NotTo(BeEmpty())
		}
	})

	It("writes back out as a directory that loads to the same version", func() {
		out := GinkgoT().TempDir()
		Expect(workflow.Write(out, loaded, skills)).To(Succeed())

		again, againSkills, err := workflow.Load(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetVersion()).To(Equal(loaded.GetVersion()))
		Expect(againSkills).To(HaveLen(len(skills)))
		for i, skill := range againSkills {
			Expect(skill.GetDigest()).To(Equal(skills[i].GetDigest()))
			Expect(skill.GetDescription()).To(Equal(skills[i].GetDescription()))
		}
	})

	It("no skill waits for a human", func() {
		for _, skill := range skills {
			body := skill.GetBody()
			for _, forbidden := range []string{"AskUserQuestion", "ExitPlanMode", "ask the user", "wait for confirmation"} {
				Expect(body).NotTo(ContainSubstring(forbidden),
					"skill %s was adapted for unattended use", skill.GetName())
			}
		}
	})
})
