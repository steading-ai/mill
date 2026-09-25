package acceptance_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// implementReached bounds how long the first run has to get as far as the
// implement step, which is two sessions in.
const implementReached = 6 * time.Minute

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: A killed worker's node is resumed by the next run
	//   Given a run whose worker was killed mid-step
	//   When the node is run again within the scenario's patience
	//   Then the second run resumes at the interrupted step and delivers
	It("A killed worker's node is resumed by the next run", func(ctx SpecContext) {
		needsForge()

		s := typo.stage(ctx)

		By("given a run whose worker was killed mid-step")
		first := s.start(ctx, runOptions{})
		// The ledger is what says the step is in flight, not the log: a
		// worker that has written `running` has a session the resume has to
		// find again (docs/ledger.md §Resume).
		killed := runningStep(ctx, s.NodeRef, "implement", implementReached)
		first.kill()
		Expect(first.running()).To(BeFalse())

		afterKill := workspaceDirs(s.WorkspaceDir)
		Expect(afterKill).To(HaveLen(1), "the killed run left exactly one workspace behind")

		// A killed worker releases nothing, and skift hands the node to
		// nobody while the lease is live: the second claim would be refused
		// until the TTL passed, which is longer than this scenario waits.
		// Taking the lease is what an operator does for a worker that is not
		// coming back, and it changes nothing the resume reads — the next
		// claim resumes from the ledger, not from the release reason
		// (docs/ledger.md §Resume, docs/architecture.md §Heartbeat).
		s.Graph.preempt(ctx, s.Graph.leaseID(ctx, s.NodeRef),
			"the acceptance suite taking the lease from a killed worker")

		By("when the node is run again within the scenario's patience")
		second := s.start(ctx, runOptions{})
		// Every directory that appears while the second run is in flight is
		// collected, because the workspace is destroyed when the run
		// finishes and a second one created and destroyed would otherwise
		// leave no trace.
		watcher := watchWorkspaces(s.WorkspaceDir, afterKill)
		exit := second.wait(deliveredTimeout)
		seen := watcher.stop()
		Expect(exit).To(BeZero(), "the second run completed the node")

		By("then the second run resumes at the interrupted step")
		Expect(seen).To(HaveLen(1), "the resume reattached rather than cloning again")
		Expect(seen).To(HaveKey(afterKill[0]))

		By("and both attempts ran in one session")
		detail := inspect(ctx, s.NodeRef)
		tries := attempts(detail, "implement")
		Expect(len(tries)).To(BeNumerically(">=", 2), "implement was attempted twice")

		aborted := tries[0]
		Expect(aborted.GetStepRun().GetAttempt()).To(Equal(int64(1)))
		Expect(aborted.GetStepRun().GetId()).To(Equal(killed.GetId()), "the killed attempt is attempt 1")
		Expect(aborted.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))

		resumed := tries[len(tries)-1]
		Expect(resumed.GetStepRun().GetAttempt()).To(Equal(int64(2)))
		Expect(resumed.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(resumed.GetStepRun().GetSessionId()).To(Equal(aborted.GetStepRun().GetSessionId()),
			"both attempts ran in the same session")
		Expect(resumed.GetStepRun().GetSessionId()).NotTo(BeEmpty())

		By("and delivers")
		Expect(s.Graph.show(ctx, s.NodeRef).Node.Status).To(Equal("done"))
		Expect(s.Project.file(ctx, "README.md", s.Project.DefaultBranch)).To(ContainSubstring(correction))
	})
})
