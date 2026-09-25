package cli_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("mill cancel", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("sets the flag the worker reads at its next heartbeat and changes nothing else", func() {
		r := openRun(ctx, "millcli#40", nodeID(40))
		r.closeGatherContext(ctx, "sha256:"+strings.Repeat("a", 64))
		r.start(ctx, "implement", millv1.StepKind_STEP_KIND_SKILL)

		out := r.cli(ctx, "cancel", "millcli#40", "--reason", "the operator changed their mind")

		Expect(out).To(ContainSubstring("cancel requested on millcli#40"))
		Expect(out).To(ContainSubstring("at step implement"))

		run := r.reload(ctx)
		Expect(run.GetCancelRequested()).To(BeTrue())
		// A cancellation is a request, not a transition: the run keeps its
		// status, its workflow and its branch so the next claim resumes it.
		Expect(run.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
		Expect(run.GetWorkflowId()).To(Equal(r.workflow.GetId()))
		Expect(run.GetBranch()).NotTo(BeEmpty())
	})

	It("takes the run's own id as readily as its node reference", func() {
		r := openRun(ctx, "millcli#41", nodeID(41))

		out := r.cli(ctx, "cancel", r.run.GetId())

		Expect(out).To(ContainSubstring("millcli#41"))
		Expect(r.reload(ctx).GetCancelRequested()).To(BeTrue())
	})

	It("refuses a run that has already ended, naming what it is", func() {
		r := openRun(ctx, "millcli#42", nodeID(42))
		_, err := r.store.Fail(ctx, connectRequest(&millv1.FailRequest{
			LeaseId: r.lease, RunId: r.run.GetId(), Reason: "the spec ended it",
		}))
		Expect(err).NotTo(HaveOccurred())

		out, err := runCLI(ctx, r.mill.socket, "cancel", "millcli#42")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("failed"))
	})
})
