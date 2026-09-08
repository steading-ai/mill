package run_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("Restart", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and the run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("Restart is a new generation, not a resume", func() {
		// Given a parked run at generation 1 with session implement.
		// `implement` names no on_failure, so the first failure is the one
		// that reaches a human.
		f.script(ok("context gathered"), failed("I cannot make sense of this"))
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		first := f.runOf(ctx)
		Expect(first.GetGeneration()).To(Equal(int64(1)))
		Expect(first.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		was := sessionsOf(f.inspect(ctx), "implement")
		Expect(was).NotTo(BeEmpty())

		// The park gate is the human's to dispose of; mill never touches
		// one it filed, and dagger would not let the node be claimed again
		// while it is open.
		f.approveGate(ctx, f.gatesOn(ctx)[0].GetReference())

		// When restart is invoked.
		second, err := f.runner().Restart(ctx, f.nodeRef, "restart")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.GetGeneration()).To(Equal(int64(2)))

		// Generation 1 keeps its record.
		before := f.inspectRun(ctx, first.GetId())
		Expect(before.GetSummary().GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_ABANDONED))
		Expect(before.GetSteps()).To(HaveLen(len(f.inspectRun(ctx, first.GetId()).GetSteps())))
		Expect(sessionsOf(before, "implement")).To(Equal(was))

		// The branch is gone and the pull request is closed with a comment
		// naming the restart.
		Expect(f.forge.branchesDeleted()).To(ContainElement(first.GetBranch()))
		Expect(f.forge.closingComments()).To(HaveLen(1))
		Expect(f.forge.closingComments()[0]).To(ContainSubstring("restarted"))

		// Then generation 2's implement session id is not generation 1's.
		adapter := fake.New(quickFixScripts()...)
		f.harness = adapter
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		now := sessionsOf(f.inspect(ctx), "implement")
		Expect(now).NotTo(BeEmpty())
		Expect(now).NotTo(ContainElement(was[0]))
		Expect(f.runOf(ctx).GetGeneration()).To(Equal(int64(2)))

		// And a Resume of generation 1's id is never issued.
		for _, call := range adapter.Calls() {
			Expect(call.Session.ID).NotTo(Equal(was[0]),
				"generation 1's transcript is the thing restart exists to leave behind")
		}
	})
})
