package run_test

import (
	"bytes"
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
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

	// parkedAtImplement is a run parked at generation 1 with its gate
	// answered, so the node is claimable again once a generation exists.
	parkedAtImplement := func() *millv1.Run {
		GinkgoHelper()
		f.script(ok("context gathered"), failed("I cannot make sense of this"))
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())
		first := f.runOf(ctx)
		Expect(first.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		f.approveGate(ctx, f.gatesOn(ctx)[0].GetReference())
		return first
	}

	// abandonedByHand is what a restart that stopped after the abandon
	// leaves: generation 1 abandoned, no run live, nothing created.
	abandonedByHand := func(first *millv1.Run) {
		GinkgoHelper()
		_, err := f.store.Abandon(ctx, connect.NewRequest(&millv1.AbandonRequest{RunId: first.GetId(), Reason: "restart"}))
		Expect(err).NotTo(HaveOccurred())
	}

	// Scenario: A restart that stopped after the abandon is finished by running it again
	It("A restart that stopped after the abandon is finished by running it again", func() {
		By("given a node whose generation 1 is abandoned and nothing replaced it")
		first := parkedAtImplement()
		abandonedByHand(first)

		By("when mill restart is invoked for the node")
		out := &bytes.Buffer{}
		next, err := f.runner(func(o *run.Options) { o.Out = out }).Restart(ctx, f.nodeRef, "restart")
		Expect(err).NotTo(HaveOccurred())

		By("then generation 2 is created and the output says the restart was finished")
		Expect(next.GetGeneration()).To(Equal(int64(2)))
		Expect(f.runOf(ctx).GetId()).To(Equal(next.GetId()))
		Expect(out.String()).To(ContainSubstring("finishing its restart"))
		Expect(f.forge.closingComments()).To(BeEmpty(), "nothing is closed twice")
	})

	// Scenario: A claim on a node whose run was abandoned opens the next generation
	It("A claim on a node whose run was abandoned opens the next generation", func() {
		By("given a node whose generation 1 is abandoned and nothing replaced it")
		first := parkedAtImplement()
		abandonedByHand(first)

		By("when a worker claims the node")
		f.harness = fake.New(f.quickFixScripts()...)
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the run it opens is generation 2, and generation 1 keeps its record")
		Expect(f.runOf(ctx).GetGeneration()).To(Equal(int64(2)))
		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_ABANDONED))
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
		Expect(before.GetSummary().GetWorkspace().GetState()).To(Equal(millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED),
			"the discarded workspace's row records its end")

		// The branch is gone and the pull request is closed with a comment
		// naming the restart.
		Expect(f.forge.branchesDeleted()).To(ContainElement(first.GetBranch()))
		Expect(f.forge.closingComments()).To(HaveLen(1))
		Expect(f.forge.closingComments()[0]).To(ContainSubstring("restarted"))

		// Then generation 2's implement session id is not generation 1's.
		adapter := fake.New(f.quickFixScripts()...)
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
