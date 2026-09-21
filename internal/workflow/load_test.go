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
    timeout: 10m
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

	It("defaults a skill's retries to three and a check's to zero", func() {
		// runCheck takes no feedback from one attempt to the next, so a
		// check that wants a retry says so; a skill gets one worth trying
		// again by default (docs/domain-model.md §Step, `retries`).
		loaded, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
  - name: check
    timeout: 10m
    kind: check
    check: {command: [scripts/check]}
    evidence: exit
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(step(loaded, "implement").GetRetries()).To(BeNumerically("==", 3))
		Expect(step(loaded, "check").GetRetries()).To(BeZero())
	})

	It("A step without a budget does not load", func() {
		By("given a workflow definition with a step lacking timeout")
		root := dir(`
name: unbudgeted
steps:
  - name: gather-context
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    timeout: 10m
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."})

		By("when the loader reads it")
		_, _, err := workflow.Load(root)

		By("then loading fails and the error names the step")
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("workflow unbudgeted")))
		Expect(err).To(MatchError(ContainSubstring("step implement: timeout is required")))
		Expect(err.Error()).NotTo(ContainSubstring("step gather-context"), "the step with a budget is not named")
	})

	It("refuses a timeout that is not a positive duration", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    timeout: 0s
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("step implement: timeout: 0s is not a positive duration")))
	})

	It("refuses a key it does not understand", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    flavour: opus
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("flavour")))
	})

	It("carries a step's harness options to the store as the yaml wrote them", func() {
		loaded, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    harness:
      claude:
        model: sonnet
        effort: medium
        tools:
          allow: [Glob]
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		settings := step(loaded, "implement").GetHarness()["claude"]
		Expect(settings.GetOptions().AsMap()).To(Equal(map[string]any{
			"model":  "sonnet",
			"effort": "medium",
			"tools":  map[string]any{"allow": []any{"Glob"}},
		}))
	})

	It("reads a step's token budget and model pin, and writes them back", func() {
		// The two are mill's, not the harness's: the credential gateway
		// applies them from outside the session (docs/sandbox.md
		// §Credentials), so they are fields of the step and not keys of
		// the options document.
		loaded, skills, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    token_budget: 2000000
    models: [sonnet, claude-haiku-4-5]
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(step(loaded, "implement").GetTokenBudget()).To(Equal(int64(2000000)))
		Expect(step(loaded, "implement").GetModels()).To(Equal([]string{"sonnet", "claude-haiku-4-5"}))

		out := GinkgoT().TempDir()
		Expect(workflow.Write(out, loaded, skills)).To(Succeed())
		again, _, err := workflow.Load(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetVersion()).To(Equal(loaded.GetVersion()), "a pulled definition re-loads to the version it was pulled at")

		_, _, err = workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    token_budget: -1
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("token_budget")))
	})

	It("accepts an option it has never heard of, because the harness owns the schema", func() {
		loaded, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    harness:
      claude:
        model: sonnet
        thinking_budget: 4096
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(step(loaded, "implement").GetHarness()["claude"].GetOptions().AsMap()).
			To(HaveKeyWithValue("thinking_budget", float64(4096)))
	})

	It("refuses a harness block that is not a mapping", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    harness:
      claude: sonnet
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(HaveOccurred())
	})

	It("The old key is not silently accepted", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    harness:
      claude-code:
        model: sonnet
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(HaveOccurred(), "the load is refused")
		Expect(err).To(MatchError(MatchRegexp(`harness\.claude($|[^-])`)),
			"the message names the key the adapter answers to")
	})

	It("fills the documented defaults for how a status step watches the forge's checks", func() {
		loaded, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
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
    timeout: 10m
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
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("status: interval")))
	})

	It("reads the workflow's own mcp-servers, never global, and round-trips them", func() {
		loaded, skills, err := workflow.Load(dir(`
name: tiny
mcp-servers:
  docs:
    command: docs-mcp
    args: ["--stdio"]
    env:
      TOKEN: abc
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.GetMcpServers()).To(HaveKey("docs"))
		docs := loaded.GetMcpServers()["docs"]
		Expect(docs.GetCommand()).To(Equal("docs-mcp"))
		Expect(docs.GetArgs()).To(Equal([]string{"--stdio"}))
		Expect(docs.GetEnv()).To(Equal(map[string]string{"TOKEN": "abc"}))

		out := GinkgoT().TempDir()
		Expect(workflow.Write(out, loaded, skills)).To(Succeed())
		again, _, err := workflow.Load(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetVersion()).To(Equal(loaded.GetVersion()), "a pulled definition re-loads to the version it was pulled at")
		Expect(again.GetMcpServers()).To(Equal(loaded.GetMcpServers()))
	})

	It("refuses a step whose skill directory is not there", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
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
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
  - name: implement
    timeout: 10m
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
    timeout: 10m
    kind: check
    check: {command: [scripts/check]}
    evidence: exit
    on_failure: revise implement
  - name: implement
    timeout: 10m
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
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    on_failure: revise implement
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("not earlier")))
	})

	It("lets a gate follow a step whose write is the file its artifact was read from", func() {
		// The plan gate: what the step wrote is on the node as its
		// evidence, so a worker that recreates the workspace elsewhere is
		// not missing it (docs/domain-model.md §"Gates and pushes").
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: plan
    timeout: 10m
    kind: skill
    session: implement
    skill: plan
    evidence: artifact
    artifact:
      path: docs/plans/{{.Date}}-{{.NodeSlug}}.md
    capabilities: [read, write]
  - name: plan-approval
    timeout: 10m
    kind: gate
    gate:
      ask:
        title: Approve the plan
    evidence: gate
`, map[string]string{"plan": "Write the plan."}))
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses an artifact source on a step that closes on something else", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: plan
    timeout: 10m
    kind: skill
    session: implement
    skill: plan
    evidence: commit
    artifact:
      path: docs/plans/plan.md
    capabilities: [read, write]
`, map[string]string{"plan": "Write the plan."}))
		Expect(err).To(MatchError(ContainSubstring("step plan")))
		Expect(err).To(MatchError(ContainSubstring("closes on commit")))
	})

	It("refuses an artifact path that climbs out of the checkout", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: plan
    timeout: 10m
    kind: skill
    session: implement
    skill: plan
    evidence: artifact
    artifact:
      path: ../../etc/plan.md
    capabilities: [read, write]
`, map[string]string{"plan": "Write the plan."}))
		Expect(err).To(MatchError(ContainSubstring("climbs out of the checkout")))
	})

	It("refuses a gate after a write step with no url evidence between them, naming the gate", func() {
		_, _, err := workflow.Load(dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    capabilities: [write]
  - name: merge
    timeout: 10m
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
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    capabilities: [write]
  - name: pr
    timeout: 10m
    kind: skill
    session: implement
    skill: pr
    evidence: url
    capabilities: [forge]
  - name: merge
    timeout: 10m
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
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
    evidence: structured
    capabilities: [read]
  - name: approve-plan
    timeout: 10m
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
    timeout: 10m
    kind: skill
    session: implement
    skill: implement
`, map[string]string{"implement": "Make the change."}))
		Expect(err).To(MatchError(ContainSubstring("evidence is required")))
	})

	It("loads a preparatory step that names a command and closes on exit", func() {
		loaded, _, err := workflow.Load(dir(`
name: readied
steps:
  - name: toolchain
    kind: preparatory
    check:
      command: [sh, -c, "mise trust /work && mise install"]
    evidence: exit
    timeout: 20m
  - name: implement
    kind: skill
    session: implement
    skill: implement
    evidence: artifact
    timeout: 10m
`, map[string]string{"implement": "Make the change."}))
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.GetSteps()[0].GetKind()).To(Equal(millv1.StepKind_STEP_KIND_PREPARATORY))
		Expect(loaded.GetSteps()[0].GetCheck().GetCommand()).To(HaveLen(3))
	})

	It("refuses a preparatory step written as a check: a pass code, a stdout shape, or feedback", func() {
		_, _, err := workflow.Load(dir(`
name: readied
steps:
  - name: toolchain
    kind: preparatory
    check:
      command: [mise, install]
      exit: [0]
      feedback: revise
    evidence: exit
    timeout: 20m
`, nil))
		Expect(err).To(MatchError(ContainSubstring("step toolchain: a preparatory step names only a command")))
	})

	It("refuses a preparatory step with no command, or one that closes on anything but exit", func() {
		_, _, err := workflow.Load(dir(`
name: readied
steps:
  - name: toolchain
    kind: preparatory
    evidence: artifact
    timeout: 20m
`, nil))
		Expect(err).To(MatchError(ContainSubstring("step toolchain: a preparatory step names a command")))
		Expect(err).To(MatchError(ContainSubstring("step toolchain: a preparatory step closes on exit evidence")))
	})

	It("refuses retries or on_failure on a preparatory step, which is never failed", func() {
		_, _, err := workflow.Load(dir(`
name: readied
steps:
  - name: toolchain
    kind: preparatory
    check:
      command: [mise, install]
    evidence: exit
    retries: 2
    timeout: 20m
`, nil))
		Expect(err).To(MatchError(ContainSubstring("step toolchain: a preparatory step never fails, so it has no retries and no on_failure")))
	})

	It("refuses a preparatory step that names a session", func() {
		_, _, err := workflow.Load(dir(`
name: readied
steps:
  - name: toolchain
    kind: preparatory
    session: implement
    check:
      command: [mise, install]
    evidence: exit
    timeout: 20m
`, nil))
		Expect(err).To(MatchError(ContainSubstring("step toolchain: a preparatory step runs no session and files no gate")))
	})

	It("refuses a skill whose frontmatter carries a field the spec does not allow", func() {
		root := dir(`
name: tiny
steps:
  - name: implement
    timeout: 10m
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
    timeout: 10m
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

	It("loads with the toolchain step and the six steps of the Quick Fix tier, in order", func() {
		names := []string{}
		for _, s := range loaded.GetSteps() {
			names = append(names, s.GetName())
		}
		Expect(names).To(Equal([]string{"toolchain", "gather-context", "implement", "commit", "clean-tree", "pr", "merge"}))
	})

	It("readies the machine with a preparatory toolchain step under its own budget", func() {
		s := step(loaded, "toolchain")
		Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_PREPARATORY))
		Expect(s.GetCheck().GetCommand()).To(Equal([]string{"sh", "-c", "mise trust /work && mise install"}))
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
		Expect(s.GetTimeout().AsDuration()).To(Equal(20 * time.Minute))
		Expect(s.GetSession()).To(BeEmpty(), "a preparatory step runs no session")
		Expect(s.GetRetries()).To(BeZero(), "and is never failed, so has no retries")
		Expect(s.GetOnFailure()).To(BeNil(), "and no on_failure")
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

	It("clean-tree is a check whose failure revises commit, and takes no retries of its own", func() {
		s := step(loaded, "clean-tree")
		Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_CHECK))
		Expect(s.GetCheck().GetCommand()).To(Equal([]string{"scripts/clean-tree"}))
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_EXIT))
		Expect(s.GetOnFailure().GetKind()).To(Equal(millv1.OnFailureKind_ON_FAILURE_KIND_REVISE))
		Expect(s.GetOnFailure().GetReviseTarget()).To(Equal("commit"))
		Expect(s.GetRetries()).To(BeZero(), "explicit, and it is also the check default")
	})

	It("merge is a gate that auto-approves, closes on a status, and sends a revise back to implement", func() {
		s := step(loaded, "merge")
		Expect(s.GetKind()).To(Equal(millv1.StepKind_STEP_KIND_GATE))
		Expect(s.GetGate().GetAuto()).To(Equal("true"), "repo protections are the repo's job")
		Expect(s.GetGate().GetAsk().GetTitle()).NotTo(BeEmpty())
		Expect(s.GetEvidence()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_STATUS))
		Expect(s.GetOnFailure().GetKind()).To(Equal(millv1.OnFailureKind_ON_FAILURE_KIND_REVISE))
		Expect(s.GetOnFailure().GetReviseTarget()).To(Equal("implement"))
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
