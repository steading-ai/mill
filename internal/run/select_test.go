package run_test

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// Selection's second rule, driven end to end: a project with no default
// assesses the node before it walks anything (docs/domain-model.md
// §Selection).
//
// The assessing session is scripted, as every session in this suite is. What
// is real is everything the rule is about: the step runs in the ledger, the
// schema the output is held to, the scope the named workflow is resolved in,
// and the tier the run then carries into its gates.

// buildWorkflowDir is the definition this repository seeds the store with.
const buildWorkflowDir = "../../workflows/build-workflow"

// assessed is what an assessing session emits: the four fields the schema
// beside the skill requires. Signals are []any rather than []string because
// that is what a harness's structured output arrives as, and what the
// validator is given.
func assessed(workflow, tier string, signals ...string) fake.Script {
	list := make([]any, 0, len(signals))
	for _, signal := range signals {
		list = append(list, signal)
	}
	return fake.Script{Terminal: fake.Terminal("assessed", map[string]any{
		"workflow":  workflow,
		"tier":      tier,
		"signals":   list,
		"reasoning": "One line of README.md, no interface and no new behaviour.",
	}, usage(3))}
}

// malformed is a session whose structured output does not satisfy the
// schema.
func malformed(output map[string]any) fake.Script {
	return fake.Script{Terminal: fake.Terminal("assessed", output, usage(3))}
}

var _ = Describe("Selection by assessment", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and the run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.pushWorkflowFrom(ctx, buildWorkflowDir)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	// noDefault is the whole of "a project with no default": the worker is
	// configured with no workflow to fall back on, which is what makes the
	// assessment the rule that answers.
	noDefault := func(o *run.Options) {
		o.Workflow = ""
		o.Tier = millv1.Tier_TIER_UNSPECIFIED
	}

	// Scenario: The assessment chooses
	//   Given a node in a project with no default
	//   When the run starts
	//   Then the assess step runs first
	//   And the select step's evidence carries the assessment's workflow,
	//   tier and signals
	It("The assessment chooses", func() {
		f.script(append([]fake.Script{assessed("quick-fix", "quick-fix",
			"README.md is the only file the ask names",
			"no test exists for it and the ask needs none",
			"the ask names no interface or design change")},
			f.quickFixScripts()...)...)

		Expect(f.runner(noDefault).Execute(ctx, f.nodeRef)).To(Succeed())

		detail := f.inspect(ctx)
		Expect(detail.GetSteps()).NotTo(BeEmpty())
		Expect(detail.GetSteps()[0].GetStepRun().GetStepName()).To(Equal("assess"),
			"the assessment is step 0 of the run")
		Expect(detail.GetSteps()[1].GetStepRun().GetStepName()).To(Equal("select"),
			"the selection is recorded on what the assessment said")
		Expect(closedSteps(detail)).To(HaveKeyWithValue("assess", millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED))

		selection := evidenceOf(detail, "select")
		Expect(selection.GetKind()).To(Equal(millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED))
		Expect(selection.GetVerifiedBy()).To(Equal("mill: assessment"))
		chosen := selection.GetStructured().AsMap()
		Expect(chosen).To(HaveKeyWithValue("basis", "assessment"))
		Expect(chosen).To(HaveKeyWithValue("workflow", "quick-fix"))
		Expect(chosen).To(HaveKeyWithValue("tier", "quick-fix"))
		Expect(chosen).To(HaveKeyWithValue("reasoning", ContainSubstring("no interface")))
		Expect(chosen["signals"]).To(HaveLen(3))
		Expect(chosen["signals"]).To(ContainElement("README.md is the only file the ask names"))

		Expect(f.runOf(ctx).GetWorkflowId()).To(Equal(f.workflow.GetId()),
			"the run follows the workflow the assessment named")
		Expect(f.runOf(ctx).GetTier()).To(Equal(millv1.Tier_TIER_QUICK_FIX))
		Expect(closedSteps(detail)).To(HaveKey("implement"), "the run then walked the selected workflow")
	})

	// Scenario: The assessment runs under a step slice
	//   Given a workspace with a step boundary
	//   When the assessing session is spawned
	//   Then a slice has been begun for it, and it is ended when the
	//   assessment closes
	//
	// A machine refuses an exec under no slice, and the assessing session
	// is spawned through the workspace like every other; an assessment
	// outside the boundary fails on a machine before the session says a
	// word (docs/sandbox.md §Exec, §"Step boundary").
	It("The assessment runs under a step slice", func() {
		var ws *sliced
		f.wrapWorkspace = func(inner run.Workspace) run.Workspace {
			ws = &sliced{Workspace: inner}
			return ws
		}
		var underSlice int
		assess := assessed("quick-fix", "quick-fix", "README.md is the only file the ask names")
		assess.Do = func(harness.Session) { underSlice = ws.active() }
		f.script(append([]fake.Script{assess}, f.quickFixScripts()...)...)

		Expect(f.runner(noDefault).Execute(ctx, f.nodeRef)).To(Succeed())

		Expect(underSlice).To(Equal(1), "the assessing session ran under the run's first slice")
		Expect(ws.begun()).NotTo(BeEmpty())
		Expect(ws.begun()[0]).To(Equal(1))
		Expect(ws.ended()).To(Equal(ws.begun()), "every slice begun was ended, the assessment's first")
		Expect(ws.closedAt(1)).To(BeTrue(), "the assessment closed on evidence, so its slice left a candidate")
		Expect(closedSteps(f.inspect(ctx))).To(HaveKeyWithValue("assess", millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED))
	})

	// Scenario: An assessment cannot invent a workflow
	//   Given an assessment naming a workflow that is not in scope
	//   When the select step verifies it
	//   Then the run fails at select
	//   And the message names the workflow and the scope
	It("An assessment cannot invent a workflow", func() {
		f.script(assessed("docs", "quick-fix", "the ask is a document"))

		err := f.runner(noDefault).Execute(ctx, f.nodeRef)

		Expect(err).To(MatchError(ContainSubstring(`workflow "docs"`)))
		Expect(err).To(MatchError(ContainSubstring("not in scope")))
		Expect(err).To(MatchError(ContainSubstring(project)), "the message names the scope it was looked for in")
		Expect(err).To(MatchError(ContainSubstring("quick-fix")), "and what that scope does have")

		detail := f.inspect(ctx)
		Expect(stepRuns(detail, "select")).To(HaveLen(1))
		Expect(stepRuns(detail, "select")[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(closedSteps(detail)).NotTo(HaveKey("select"))
		Expect(f.runOf(ctx).GetWorkflowId()).To(BeEmpty(), "the run has no workflow")
		for _, step := range []string{"gather-context", "implement", "commit", "pr"} {
			Expect(stepRuns(detail, step)).To(BeEmpty(), "the run walked "+step)
		}

		By("and the run is failed, with the node held rather than claimed again")
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		node, err := f.client.Node(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetHold().GetReason()).To(ContainSubstring("not in scope"))
	})

	Describe("Output that does not match the schema", func() {
		It("fails the step as malformed, naming the field", func() {
			f.script(malformed(map[string]any{
				"workflow":  "quick-fix",
				"tier":      "enormous",
				"signals":   []any{"README.md"},
				"reasoning": "because",
			}))

			err := f.runner(noDefault).Execute(ctx, f.nodeRef)

			Expect(err).To(MatchError(ContainSubstring("/tier")))
			Expect(f.runOf(ctx).GetWorkflowId()).To(BeEmpty())
			Expect(stepRuns(f.inspect(ctx), "select")).To(BeEmpty(), "no selection was recorded")
		})

		It("treats an empty signals list as malformed", func() {
			f.script(malformed(map[string]any{
				"workflow":  "quick-fix",
				"tier":      "quick-fix",
				"signals":   []any{},
				"reasoning": "because",
			}))

			err := f.runner(noDefault).Execute(ctx, f.nodeRef)

			Expect(err).To(MatchError(ContainSubstring("/signals")))
			detail := f.inspect(ctx)
			Expect(closedSteps(detail)).NotTo(HaveKey("assess"))
			Expect(stepRuns(detail, "assess")).To(HaveLen(3), "the step spent its own retries and no more")

			By("and the run is failed, with the node held rather than reclaimed into the same failure")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
			node, err := f.client.Node(ctx, f.nodeRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.GetHold().GetReason()).To(ContainSubstring("the assessment did not close"))
		})
	})

	// The tier is the reason the assessment is worth recording: it is what a
	// gate's `auto` predicate compares against, and until this node it could
	// only come from the project default.
	Describe("The tier a gate predicate reads", func() {
		// tiered is quick-fix with a merge gate that auto-approves a quick
		// fix and asks a person about anything larger.
		var tiered string

		BeforeEach(func() {
			tiered = f.variant(ctx, "tiered-fix", func(definition *millv1.Workflow) {
				stepOf(definition, "merge").GetGate().Auto = "tier <= quick-fix"
			})
		})

		It("refuses to auto-approve a merge the assessment called a feature", func() {
			f.script(append([]fake.Script{assessed(tiered, "feature",
				"three packages are touched", "the ask names a new interface")},
				f.quickFixScripts()...)...)

			Expect(f.runner(noDefault).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetTier()).To(Equal(millv1.Tier_TIER_FEATURE))
			Expect(f.gatesOn(ctx)).To(HaveLen(1), "a feature is more than the policy allows")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_WAITING))
		})

		It("auto-approves the same merge when the assessment called it a quick fix", func() {
			f.script(append([]fake.Script{assessed(tiered, "quick-fix",
				"one line of README.md", "no interface changes")},
				f.quickFixScripts()...)...)

			Expect(f.runner(noDefault).Execute(ctx, f.nodeRef)).To(Succeed())

			Expect(f.runOf(ctx).GetTier()).To(Equal(millv1.Tier_TIER_QUICK_FIX))
			Expect(f.gatesOn(ctx)).To(BeEmpty(), "nothing was filed in skift")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		})
	})

	Describe("The assessing session", func() {
		It("is told which workflows it may choose between, and given the step's schema", func() {
			f.script(append([]fake.Script{assessed("quick-fix", "quick-fix", "README.md only")},
				f.quickFixScripts()...)...)

			Expect(f.runner(noDefault).Execute(ctx, f.nodeRef)).To(Succeed())

			first := f.harness.Calls()[0]
			Expect(first.Prompt.Text).To(ContainSubstring("The workflows in scope"))
			Expect(first.Prompt.Text).To(ContainSubstring("`quick-fix`"))
			Expect(first.Prompt.Text).NotTo(ContainSubstring("`build-workflow`"),
				"the assessment does not choose itself")
			Expect(first.Prompt.OutputSchema).To(ContainSubstring(`"signals"`))
			Expect(first.Session.Options).To(HaveKeyWithValue("model", "sonnet"))
		})
	})
})

// Scenario: A project default naming no workflow fails the run and holds
// the node
//
//	Given a project default pinned to a workflow that does not exist
//	When the run starts
//	Then the run fails, naming the workflow
//	And the node is held rather than claimed again
var _ = Describe("Selection by project default", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("A project default naming no workflow fails the run and holds the node", func() {
		err := f.runner(func(o *run.Options) { o.Workflow = "no-such-workflow" }).Execute(ctx, f.nodeRef)

		Expect(err).To(MatchError(ContainSubstring(`no workflow named "no-such-workflow"`)))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		node, err := f.client.Node(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetHold().GetReason()).To(ContainSubstring(`no workflow named "no-such-workflow"`))
	})
})

// sliced is a workspace with a step boundary that records every slice begun
// and ended, whether each closed on evidence, and which one is active.
type sliced struct {
	run.Workspace
	mu     sync.Mutex
	slice  int
	begins []int
	ends   []int
	closed map[int]bool
}

var _ workspace.StepBoundary = (*sliced)(nil)

func (s *sliced) BeginStep(_ context.Context, ordinal int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slice = ordinal
	s.begins = append(s.begins, ordinal)
	return nil
}

func (s *sliced) EndStep(_ context.Context, ordinal int, closedOnEvidence bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed == nil {
		s.closed = map[int]bool{}
	}
	s.slice = 0
	s.ends = append(s.ends, ordinal)
	s.closed[ordinal] = closedOnEvidence
	return nil
}

// active is the ordinal of the slice begun and not yet ended; 0 for none.
func (s *sliced) active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slice
}

func (s *sliced) begun() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.begins...)
}

func (s *sliced) ended() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.ends...)
}

func (s *sliced) closedAt(ordinal int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed[ordinal]
}
