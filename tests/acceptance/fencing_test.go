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
	// Scenario: Fenced out, and finished by the next worker
	//   Given a run in flight holding the node's lease
	//   When the lease is expired and a second worker claims the node
	//   Then the running mill exits with the fenced exit code
	//   And a fresh run completes the node without a duplicate merge request
	It("Fenced out, and finished by the next worker", func(ctx SpecContext) {
		needsForge()

		s := typo.stage(ctx)

		By("given a run in flight holding the node's lease")
		fenced := s.start(ctx, runOptions{TTL: fencedTTL, Heartbeat: fencedHeartbeat})
		runningStep(ctx, s.NodeRef, "gather-context", implementReached)
		held := s.Graph.leaseID(ctx, s.NodeRef)
		Expect(held).NotTo(BeEmpty(), "the run holds the node's lease")

		By("when the lease is expired and a second worker claims the node")
		s.Graph.expire(ctx, held)
		taken := s.Graph.claimAs(ctx, "acceptance-second-worker")
		Expect(taken).NotTo(Equal(held), "the node is held under a lease that is not the run's")

		By("then the running mill exits with the fenced exit code")
		Expect(fenced.wait(fencedNoticed)).To(Equal(cli.ExitFenced))

		By("and a fresh run completes the node without a duplicate merge request")
		s.Graph.handBack(ctx, taken)

		Expect(s.start(ctx, runOptions{}).wait(deliveredTimeout)).To(BeZero())
		Expect(s.Graph.show(ctx, s.NodeRef).Node.Status).To(Equal("done"))

		branch := inspect(ctx, s.NodeRef).GetSummary().GetRun().GetBranch()
		// mergeRequestFor fails when there is more than one merge request on
		// the branch, which is the duplicate this scenario is about.
		Expect(mergeRequestFor(s.Project.mergeRequests(ctx), branch).State).To(Equal("merged"))
	})
})
