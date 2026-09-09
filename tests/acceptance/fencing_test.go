package acceptance_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/cli"
)

// The lease the fencing scenario gives its run, and how often it renews it.
// Production renews every five minutes; a spec that waited that long to see
// a worker notice it had been fenced would spend its whole budget on one
// assertion.
const (
	fencedTTL       = 90 * time.Second
	fencedHeartbeat = 5 * time.Second
	fencedNoticed   = 2 * time.Minute
)

var _ = Describe("Acceptance", Label("acceptance"), func() {
	// Scenario: A worker fenced out writes nothing
	//   Given a worker whose lease was taken by another
	//   When its next heartbeat is refused
	//   Then it abandons the run locally and the other worker finishes it
	It("A worker fenced out writes nothing", func(ctx SpecContext) {
		needsForge()

		s := typo.stage(ctx)

		By("given a worker whose lease was taken by another")
		fenced := s.start(ctx, runOptions{TTL: fencedTTL, Heartbeat: fencedHeartbeat})
		runningStep(ctx, s.NodeRef, "gather-context", implementReached)
		held := s.Graph.leaseID(ctx, s.NodeRef)
		Expect(held).NotTo(BeEmpty(), "the run holds the node's lease")

		s.Graph.preempt(ctx, held, "the acceptance suite taking the lease from a live worker")
		taken := s.Graph.claimAs(ctx, "acceptance-second-worker")
		Expect(taken).NotTo(Equal(held), "the node is held under a lease that is not the run's")

		By("when its next heartbeat is refused")
		Expect(fenced.wait(fencedNoticed)).To(Equal(cli.ExitFenced))

		By("then it abandons the run locally and the other worker finishes it")
		s.Graph.handBack(ctx, taken)

		Expect(s.start(ctx, runOptions{}).wait(deliveredTimeout)).To(BeZero())
		Expect(s.Graph.show(ctx, s.NodeRef).Node.Status).To(Equal("done"))

		branch := inspect(ctx, s.NodeRef).GetSummary().GetRun().GetBranch()
		// mergeRequestFor fails when there is more than one merge request on
		// the branch, which is the duplicate this scenario is about.
		Expect(mergeRequestFor(s.Project.mergeRequests(ctx), branch).State).To(Equal("merged"))
	})
})
