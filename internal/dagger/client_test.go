package dagger_test

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/testsupport"
)

// errNotYetExpired keeps a poll going until the lease's own expiry has
// passed, so the heartbeat under test is the first since the claim.
var errNotYetExpired = errors.New("the lease has not expired yet")

var _ = Describe("The dagger client", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and everything below outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	Describe("Claiming a named node", func() {
		It("takes a lease carrying harness, agent and session references", func() {
			ref := f.node(ctx, "one line change", "Change the README.", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			claimed, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimed.Node.GetReference()).To(Equal(ref))
			Expect(claimed.Lease.GetHarness()).To(Equal(dagger.HarnessName))
			Expect(claimed.Lease.GetAgentRef()).To(Equal("spec@host"))
			Expect(claimed.Lease.GetSessionRef()).To(Equal("spec"))
		})

		It("leaves the node's labels as it found them", func() {
			ref := f.node(ctx, "labelled", "body", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			_, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())

			node, err := f.mill.Node(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.GetLabels()).To(BeEmpty())
		})

		It("explains a node that is not ready rather than waiting for it", func() {
			blocker := f.node(ctx, "first", "body", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			blocked := f.node(ctx, "second", "body", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			f.link(ctx, blocker, blocked)

			_, err := f.mill.Claim(ctx, blocked)
			var notReady *dagger.NotReadyError
			Expect(err).To(BeAssignableToTypeOf(notReady))
			Expect(err.Error()).To(ContainSubstring("blocked by"))
		})
	})

	Describe("The five refusals mill makes on a user's node", func() {
		var gate string

		BeforeEach(func() {
			gate = f.node(ctx, "merge?", "Approve the merge.", daggerv1.AssigneeKind_ASSIGNEE_KIND_USER)
		})

		It("refuses to claim it", func() {
			_, err := f.mill.Claim(ctx, gate)
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to claim"))
		})

		It("refuses to complete it", func() {
			err := f.mill.Complete(ctx, "mill-spec-complete", "", gate, &daggerv1.Result{Body: "approved"})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to complete"))
		})

		It("refuses to drop it", func() {
			_, err := f.mill.ApplyPatch(ctx, "mill-spec-drop", "", &daggerv1.PatchOp{
				Op: &daggerv1.PatchOp_DropNode{DropNode: &daggerv1.DropNodeOp{
					Node: gate, Reason: daggerv1.DropReason_DROP_REASON_NO_LONGER_RELEVANT,
				}},
			})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to drop"))
		})

		It("refuses to reclassify it", func() {
			agent := daggerv1.AssigneeKind_ASSIGNEE_KIND_AGENT
			_, err := f.mill.ApplyPatch(ctx, "mill-spec-reclassify", "", &daggerv1.PatchOp{
				Op: &daggerv1.PatchOp_UpdateNode{UpdateNode: &daggerv1.UpdateNodeOp{
					Node: gate, AssigneeKind: &agent,
				}},
			})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to reclassify"))
		})

		It("refuses to unlink it", func() {
			work := f.node(ctx, "the work", "body", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			f.link(ctx, gate, work)

			_, err := f.mill.ApplyPatch(ctx, "mill-spec-unlink", "", &daggerv1.PatchOp{
				Op: &daggerv1.PatchOp_RemoveEdge{RemoveEdge: &daggerv1.RemoveEdgeOp{
					From: gate, To: work, Kind: daggerv1.EdgeKind_EDGE_KIND_BLOCKS,
				}},
			})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to unlink"))
		})

		It("refuses one it filed itself", func() {
			work := f.node(ctx, "the work", "body", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			claimed, err := f.mill.Claim(ctx, work)
			Expect(err).NotTo(HaveOccurred())

			filed, err := f.mill.CreateGate(ctx, "mill-spec-gate", claimed.Lease.GetId(), dagger.Gate{
				Title:  "Merge?",
				Body:   "Complete this node to merge.",
				Blocks: work,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(filed.GetAssigneeKind()).To(Equal(daggerv1.AssigneeKind_ASSIGNEE_KIND_USER))

			err = f.mill.Complete(ctx, "mill-spec-complete-own", claimed.Lease.GetId(),
				filed.GetReference(), &daggerv1.Result{Body: "approved"})
			Expect(err).To(MatchError(dagger.ErrUserNode))
		})

		It("is mill's refusal and not dagger's: the same principal may complete a gate", func() {
			// Mill runs as the operator's `user` principal, and dagger
			// applies no gate veto to a user. The refusal above is
			// therefore mill's own discipline, which is only worth
			// asserting beside the proof that dagger permits the act.
			_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
				RequestId: "spec-complete-gate",
				Project:   f.project,
				Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
					Node:   gate,
					Result: &daggerv1.Result{Body: "approved"},
				}}}},
			}))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Fencing", func() {
		It("reports a heartbeat on an expired lease as fenced", func() {
			ref := f.node(ctx, "short lease", "body", daggerv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			claimed, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())

			// The TTL is set by a heartbeat rather than by the claim,
			// because any write by the holder renews the lease and a claim
			// makes one of its own. Then the window passes with nothing
			// renewing it, and the next heartbeat is the one that learns.
			lease, err := f.mill.Heartbeat(ctx, claimed.Lease.GetId(), time.Second)
			Expect(err).NotTo(HaveOccurred())
			expiry := lease.GetExpiresAt().AsTime()

			Eventually(func() error {
				if time.Now().Before(expiry) {
					return errNotYetExpired
				}
				_, err := f.mill.Heartbeat(ctx, claimed.Lease.GetId(), time.Second)
				return err
			}, 10*time.Second, 200*time.Millisecond).Should(MatchError(dagger.ErrFenced))
		})
	})

	Describe("Request ids", func() {
		It("names the run, the step and the attempt, under mill's prefix", func() {
			Expect(dagger.RequestID("01RUN", "implement", 2)).To(Equal("mill-01RUN-implement-2"))
		})
	})
})
