package run_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// mill#92 (steading#17): a restart whose forge cleanup silently failed left a
// branch and its pull request stranded, and a later generation's `pr` step
// reused the branch name and, with it, the abandoned pull request. These
// specs prove the `pr` step refuses that reuse instead.
var _ = Describe("The pr step", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("does not reuse a pull request at another generation's head", func() {
		// No retry: the mismatch does not clear on its own, so a second
		// attempt would only fail the same way; one attempt is enough to
		// prove the check and keeps the spec's script and assertions simple,
		// the same way the merge gate's own no-retry specs isolate a single
		// failure (gate_test.go).
		workflow := f.variant(ctx, "pr-no-retry", func(definition *millv1.Workflow) {
			stepOf(definition, "pr").Retries = 0
		})
		logDir := GinkgoT().TempDir()

		foreign := strings.Repeat("f", 40)
		scripts := f.quickFixScripts()
		scripts[3].Do = func(session harness.Session) {
			f.commits(session)
			// Stand in for the forge still showing an earlier generation's
			// abandoned head on this branch name: the run's own push
			// landed, but the forge answers a pull request at a head this
			// push never produced.
			f.forge.setHead(foreign)
		}
		f.script(scripts...)

		Expect(f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.LogDir = logDir
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		detail := f.inspect(ctx)
		prRuns := stepRuns(detail, "pr")
		Expect(prRuns).To(HaveLen(1))
		Expect(prRuns[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))

		activities, err := run.ReadSealedActivities(logDir, f.runOf(ctx).GetId(), "pr", prRuns[0].GetAttempt())
		Expect(err).NotTo(HaveOccurred())
		ended := activities[len(activities)-1].GetAttemptEnded()
		Expect(ended).NotTo(BeNil())
		Expect(ended.GetReason()).To(ContainSubstring(f.runOf(ctx).GetBranch()), "names the branch")
		Expect(ended.GetReason()).To(ContainSubstring(foreign), "names the head the forge answered with")
	})
})
