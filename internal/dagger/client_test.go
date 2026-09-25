package dagger_test

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	"github.com/steading-ai/mill/internal/dagger"
	"github.com/steading-ai/mill/internal/testsupport"
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
			ref := f.node(ctx, "one line change", "Change the README.", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			claimed, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimed.Node.GetReference()).To(Equal(ref))
			Expect(claimed.Lease.GetHarness()).To(Equal(dagger.HarnessName))
			Expect(claimed.Lease.GetAgentRef()).To(Equal("spec@host"))
			Expect(claimed.Lease.GetSessionRef()).To(Equal("spec"))
		})

		It("carries the project the node was claimed in, with the repository it delivers to", func() {
			_, err := f.graph.UpdateProject(ctx, connect.NewRequest(&skiftv1.UpdateProjectRequest{
				RequestId: "mill-spec-vcs-url", Project: projectSlug, VcsUrl: ptr("https://gitlab.example.test/group/thing.git"),
			}))
			Expect(err).NotTo(HaveOccurred())
			ref := f.node(ctx, "delivered", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			claimed, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimed.Project.GetSlug()).To(Equal(projectSlug))
			Expect(claimed.Project.GetVcsUrl()).To(Equal("https://gitlab.example.test/group/thing.git"))
		})

		It("leaves the node's labels as it found them", func() {
			ref := f.node(ctx, "labelled", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			_, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())

			node, err := f.mill.Node(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.GetLabels()).To(BeEmpty())
		})

		It("explains a node that is not ready rather than waiting for it", func() {
			blocker := f.node(ctx, "first", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			blocked := f.node(ctx, "second", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
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
			gate = f.node(ctx, "merge?", "Approve the merge.", skiftv1.AssigneeKind_ASSIGNEE_KIND_USER)
		})

		It("refuses to claim it", func() {
			_, err := f.mill.Claim(ctx, gate)
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to claim"))
		})

		It("refuses to complete it", func() {
			err := f.mill.Complete(ctx, "mill-spec-complete", "", gate, &skiftv1.Result{Body: "approved"})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to complete"))
		})

		It("refuses to drop it", func() {
			_, err := f.mill.ApplyPatch(ctx, "mill-spec-drop", "", &skiftv1.PatchOp{
				Op: &skiftv1.PatchOp_DropNode{DropNode: &skiftv1.DropNodeOp{
					Node: gate, Reason: skiftv1.DropReason_DROP_REASON_NO_LONGER_RELEVANT,
				}},
			})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to drop"))
		})

		It("refuses to reclassify it", func() {
			agent := skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT
			_, err := f.mill.ApplyPatch(ctx, "mill-spec-reclassify", "", &skiftv1.PatchOp{
				Op: &skiftv1.PatchOp_UpdateNode{UpdateNode: &skiftv1.UpdateNodeOp{
					Node: gate, AssigneeKind: &agent,
				}},
			})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to reclassify"))
		})

		It("refuses to unlink it", func() {
			work := f.node(ctx, "the work", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			f.link(ctx, gate, work)

			_, err := f.mill.ApplyPatch(ctx, "mill-spec-unlink", "", &skiftv1.PatchOp{
				Op: &skiftv1.PatchOp_RemoveEdge{RemoveEdge: &skiftv1.RemoveEdgeOp{
					From: gate, To: work, Kind: skiftv1.EdgeKind_EDGE_KIND_BLOCKS,
				}},
			})
			Expect(err).To(MatchError(dagger.ErrUserNode))
			Expect(err.Error()).To(ContainSubstring("refusing to unlink"))
		})

		It("refuses one it filed itself", func() {
			work := f.node(ctx, "the work", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			claimed, err := f.mill.Claim(ctx, work)
			Expect(err).NotTo(HaveOccurred())

			filed, err := f.mill.CreateGate(ctx, "mill-spec-gate", claimed.Lease.GetId(), dagger.Gate{
				Title:  "Merge?",
				Body:   "Complete this node to merge.",
				Blocks: work,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(filed.GetAssigneeKind()).To(Equal(skiftv1.AssigneeKind_ASSIGNEE_KIND_USER))

			err = f.mill.Complete(ctx, "mill-spec-complete-own", claimed.Lease.GetId(),
				filed.GetReference(), &skiftv1.Result{Body: "approved"})
			Expect(err).To(MatchError(dagger.ErrUserNode))
		})

		It("is mill's refusal and not dagger's: the same principal may complete a gate", func() {
			// Mill runs as the operator's `user` principal, and dagger
			// applies no gate veto to a user. The refusal above is
			// therefore mill's own discipline, which is only worth
			// asserting beside the proof that dagger permits the act.
			_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
				RequestId: "spec-complete-gate",
				Project:   f.project,
				Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
					Node:   gate,
					Result: &skiftv1.Result{Body: "approved"},
				}}}},
			}))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Claiming across projects", func() {
		It("an unscoped client claims a ready node from either project", func() {
			second := f.secondProject(ctx)
			refA := f.node(ctx, "in the first project", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			refB := f.nodeIn(ctx, second, "in the second project", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			unscoped := f.unscoped()
			first, err := unscoped.ClaimNext(ctx, nil, nil, time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(first).NotTo(BeNil())

			another, err := unscoped.ClaimNext(ctx, nil, nil, time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(another).NotTo(BeNil())

			Expect([]string{first.Node.GetReference(), another.Node.GetReference()}).To(ConsistOf(refA, refB),
				"a worker with no project configured claims from every project")
		})

		It("a client scoped to one project does not see the other's ready node", func() {
			second := f.secondProject(ctx)
			f.nodeIn(ctx, second, "only in the second project", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			claimed, err := f.mill.ClaimNext(ctx, []string{f.project}, nil, time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimed).To(BeNil(), "the filter names only the fixture's own project, which has no ready node")
		})
	})

	Describe("Scoped", func() {
		It("carries every post-claim write and gate read for the project the claim landed in", func() {
			second := f.secondProject(ctx)
			ref := f.nodeIn(ctx, second, "elsewhere", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			lost := f.nodeIn(ctx, second, "elsewhere too", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

			unscoped := f.unscoped()
			claimed, err := unscoped.ClaimNext(ctx, []string{second}, nil, time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(claimed.Node.GetReference()).To(Equal(ref))
			Expect(claimed.Project.GetSlug()).To(Equal(second))

			// Scoped is what internal/run/run.go's drive pins the run's own
			// dagger client to right after a claim, so every write and gate
			// read that follows lands in the project the claim actually
			// came from — not the worker's own (here empty) one.
			scoped := unscoped.Scoped(claimed.Project.GetSlug())

			Expect(scoped.Comment(ctx, "mill-spec-scoped-comment", claimed.Lease.GetId(), ref, "hello")).To(Succeed())

			filed, err := scoped.CreateGate(ctx, "mill-spec-scoped-gate", claimed.Lease.GetId(), dagger.Gate{
				Title: "Merge?", Body: "Complete this node to merge.", Blocks: ref, Labels: []string{dagger.GateLabel},
			})
			Expect(err).NotTo(HaveOccurred())

			gates, err := scoped.Gates(ctx, ref, dagger.GateLabel)
			Expect(err).NotTo(HaveOccurred())
			Expect(gates).To(HaveLen(1))
			Expect(gates[0].GetReference()).To(Equal(filed.GetReference()))

			Expect(scoped.AttachText(ctx, "mill-spec-scoped-artifact", claimed.Lease.GetId(), ref,
				"plan", "the plan", "do the thing", nil)).To(Succeed())
			artifacts, err := scoped.Artifacts(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(artifacts).To(HaveLen(1))

			Expect(scoped.Complete(ctx, "mill-spec-scoped-complete", claimed.Lease.GetId(), ref,
				&skiftv1.Result{Body: "done"})).To(Succeed())

			// Hold's own lease: it fences lost, not ref, and a lease is only
			// live for the node it was taken on.
			lostClaim, err := scoped.Claim(ctx, lost)
			Expect(err).NotTo(HaveOccurred())
			Expect(scoped.Hold(ctx, "mill-spec-scoped-hold", lostClaim.Lease.GetId(), lost,
				"no repository: has no vcs_url")).To(Succeed())
			node, err := scoped.Node(ctx, lost)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.GetHold().GetReason()).To(Equal("no repository: has no vcs_url"))
		})
	})

	Describe("Fencing", func() {
		It("reports a heartbeat on an expired lease as fenced", func() {
			ref := f.node(ctx, "short lease", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)

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

	Describe("Projects, for the reconciliation sweep", func() {
		It("reads its own project by slug, with the repository it delivers to", func() {
			_, err := f.graph.UpdateProject(ctx, connect.NewRequest(&skiftv1.UpdateProjectRequest{
				RequestId: "mill-spec-vcs-url-get", Project: projectSlug, VcsUrl: ptr("https://gitlab.example.test/group/own.git"),
			}))
			Expect(err).NotTo(HaveOccurred())

			project, err := f.mill.GetProject(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(project.GetSlug()).To(Equal(projectSlug))
			Expect(project.GetVcsUrl()).To(Equal("https://gitlab.example.test/group/own.git"))
		})

		It("lists the active projects, archives one by slug, and says delete is not offered", func() {
			_, err := f.graph.CreateProject(ctx, connect.NewRequest(&skiftv1.CreateProjectRequest{
				RequestId: "mill-spec-scratch", Title: "scratch", Slug: "mill-acceptance-x-0badcafe",
			}))
			Expect(err).NotTo(HaveOccurred())

			projects, err := f.mill.ListProjects(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(slugsOf(projects)).To(ConsistOf(projectSlug, "mill-acceptance-x-0badcafe"))
			Expect(projects[0].GetCreatedAt()).NotTo(BeNil(), "the age a sweep measures from is on every project")

			Expect(f.mill.DeleteProject(ctx, "mill-acceptance-x-0badcafe")).To(MatchError(errors.ErrUnsupported),
				"the vendored contract has no project delete")

			archived, err := f.mill.ArchiveProject(ctx, "mill-acceptance-x-0badcafe")
			Expect(err).NotTo(HaveOccurred())
			Expect(archived.GetArchivedAt()).NotTo(BeNil())
			projects, err = f.mill.ListProjects(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(slugsOf(projects)).To(ConsistOf(projectSlug), "an archived project is out of the list")
		})

		It("holds a node with a reason, so it leaves the ready set until a person releases it", func() {
			ref := f.node(ctx, "lost", "body", skiftv1.AssigneeKind_ASSIGNEE_KIND_UNSPECIFIED)
			claimed, err := f.mill.Claim(ctx, ref)
			Expect(err).NotTo(HaveOccurred())

			Expect(f.mill.Hold(ctx, "mill-spec-hold", claimed.Lease.GetId(), ref, "lost sandbox: mill-x is gone")).To(Succeed())
			Expect(f.mill.Release(ctx, claimed.Lease.GetId(), skiftv1.ReleaseReason_RELEASE_REASON_CRASH, "lost sandbox")).To(Succeed())

			reasons, err := f.mill.Explain(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(reasons).To(ContainElement("the node is held"))
			node, err := f.mill.Node(ctx, ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(node.GetHold().GetReason()).To(Equal("lost sandbox: mill-x is gone"))
		})
	})
})

// slugsOf is the slugs of a project list.
func slugsOf(projects []*skiftv1.Project) []string {
	slugs := make([]string, 0, len(projects))
	for _, project := range projects {
		slugs = append(slugs, project.GetSlug())
	}
	return slugs
}

func ptr[T any](v T) *T { return &v }
