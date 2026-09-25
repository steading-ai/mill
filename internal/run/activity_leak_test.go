package run_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// mill#30: an error returned between an attempt's writer opening
// (openActivity/reopenActivity) and its ending (endActivity/abortActivity)
// must still close the writer and detach the verb sink, whatever failed.
// Each spec here fails a different write on that stretch and checks both
// halves of the leak through [fixture.writerLeaked]: the activity file is no
// longer open, and a verb run afterward, through the workspace the dead
// attempt left behind, is not appended to its record.
var _ = Describe("An attempt's writer and sink on a failed write", Label(testsupport.DBLabel), func() {
	var (
		f      *fixture
		ctx    context.Context
		logDir string
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
		f.script(f.quickFixScripts()...)
		logDir = GinkgoT().TempDir()
	})

	// toolchain is the first step every run takes (workflows/quick-fix), so
	// it is the attempt every failingStore refusal here lands on: nothing
	// upstream of it has a chance to consume the RPC first.
	const toolchain = "toolchain"

	assertLeakFree := func() {
		GinkgoHelper()
		f.assertWriterClosed(ctx, logDir, toolchain)
	}

	Describe("Before the attempt executes", func() {
		It("closes the writer and detaches the sink when IssueStepToken fails", func() {
			store := &failingStore{StoreServiceClient: f.store, refuse: "IssueStepToken"}
			err := f.runner(func(o *run.Options) {
				o.Store = store
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			assertLeakFree()
		})

		It("closes the writer and detaches the sink when Rotate fails", func() {
			f.wrapWorkspace = func(ws run.Workspace) run.Workspace { return rotateFailing{Workspace: ws} }
			err := f.runner(func(o *run.Options) {
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			assertLeakFree()
		})
	})

	Describe("Between executing the attempt and deciding it", func() {
		It("closes the writer and detaches the sink when recordInstalls fails", func() {
			f.wrapWorkspace = func(ws run.Workspace) run.Workspace { return installing{Workspace: ws} }
			store := &failingStore{StoreServiceClient: f.store, refuse: "RecordInstall"}
			err := f.runner(func(o *run.Options) {
				o.Store = store
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			assertLeakFree()
		})

		It("closes the writer and detaches the sink when recordUsage fails", func() {
			// recordUsage no-ops on a step with nothing billed (finish.go),
			// which toolchain never has: gather-context is the first step
			// whose scripted turn (quickFixScripts) reports usage.
			store := &failingStore{StoreServiceClient: f.store, refuse: "RecordUsage"}
			err := f.runner(func(o *run.Options) {
				o.Store = store
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			f.assertWriterClosed(ctx, logDir, "gather-context")
		})

		It("closes the writer and detaches the sink when RevokeStepToken fails", func() {
			store := &failingStore{StoreServiceClient: f.store, refuse: "RevokeStepToken"}
			err := f.runner(func(o *run.Options) {
				o.Store = store
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			assertLeakFree()
		})

		It("closes the writer and detaches the sink when postUsage fails", func() {
			store := &failingStore{StoreServiceClient: f.store, refuse: "MarkUsagePosted"}
			err := f.runner(func(o *run.Options) {
				o.Store = store
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			assertLeakFree()
		})

		It("closes the writer and detaches the sink when reviewed's verdict attach fails", func() {
			// quick-fix declares no verdict step, so this spec adds one: a
			// review after pr, whose own AttachText this run's dagger
			// client refuses. "review-" alone would also catch a
			// review-plan step's attach in a workflow that had one; this
			// workflow does not, but the exclusion documents the seam a
			// spec that added one would need to mind.
			workflow := f.variant(ctx, "reviewed-quick-fix", func(definition *millv1.Workflow) {
				review := reviewStep(stepOf(definition, "implement").GetSkill())
				review.Evidence = millv1.EvidenceKind_EVIDENCE_KIND_VERDICT
				insertAfter(definition, "pr", review)
			})
			client := f.failingPatches(func(op *skiftv1.PatchOp) bool {
				name := op.GetAttachArtifact().GetArtifact().GetName()
				return strings.HasPrefix(name, "review-") && !strings.HasPrefix(name, "review-plan-")
			})
			f.script(append(f.quickFixScripts(), fake.Script{
				Terminal: fake.Terminal("", approves("AC-1", "looks good"), usage(5)),
			})...)

			err := f.runner(func(o *run.Options) {
				o.Workflow = workflow
				o.Dagger = client
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			f.assertWriterClosed(ctx, logDir, "review")
		})
	})

	Describe("select.go's assess step", func() {
		// noDefault forces assessment: the same rule select_test.go's specs
		// use to make assess() the first step a run takes.
		noDefault := func(o *run.Options) {
			o.Workflow = ""
			o.Tier = millv1.Tier_TIER_UNSPECIFIED
		}

		It("closes the writer and detaches the sink on assess's own IssueStepToken failure", func() {
			f.pushWorkflowFrom(ctx, buildWorkflowDir)
			f.script(append([]fake.Script{assessed("quick-fix", "quick-fix",
				"README.md is the only file the ask names")}, f.quickFixScripts()...)...)
			store := &failingStore{StoreServiceClient: f.store, refuse: "IssueStepToken"}

			err := f.runner(noDefault, func(o *run.Options) {
				o.Store = store
				o.LogDir = logDir
			}).Execute(ctx, f.nodeRef)
			Expect(err).To(HaveOccurred())
			f.assertWriterClosed(ctx, logDir, "assess")
		})
	})
})
