package dagger_test

import (
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/gen/dagger/v1/daggerv1connect"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/testsupport"
)

// fixture is a running dagger with a project in it, mill's client, and the
// generated client underneath. Two clients on purpose: the point of several
// specs is that mill's refusals are mill's own, which is only demonstrable
// beside a client that does not make them.
type fixture struct {
	service *testsupport.Dagger
	project string
	mill    *dagger.Client
	graph   daggerv1connect.GraphServiceClient
	work    daggerv1connect.WorkServiceClient
}

const projectSlug = "millspec"

// start brings up dagger and creates the spec's project.
func start(ctx context.Context) *fixture {
	GinkgoHelper()
	service := pg.StartDagger(ctx)
	client := service.HTTPClient()
	f := &fixture{
		service: service,
		project: projectSlug,
		mill: dagger.New(client, service.BaseURL(), projectSlug,
			dagger.WithAgentRef("spec@host"), dagger.WithSessionRef("spec")),
		graph: daggerv1connect.NewGraphServiceClient(client, service.BaseURL()),
		work:  daggerv1connect.NewWorkServiceClient(client, service.BaseURL()),
	}
	_, err := f.graph.CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{
		RequestId: "mill-spec-project",
		Title:     "mill specs",
		Slug:      projectSlug,
	}))
	Expect(err).NotTo(HaveOccurred())
	return f
}

// node creates a node in the project and returns its reference.
func (f *fixture) node(ctx context.Context, title, body string, assignee daggerv1.AssigneeKind) string {
	GinkgoHelper()
	res, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "spec-" + title,
		Project:   f.project,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CreateNode{CreateNode: &daggerv1.CreateNodeOp{
			Title:        title,
			Body:         body,
			AssigneeKind: assignee,
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	Expect(res.Msg.GetNodes()).To(HaveLen(1))
	return res.Msg.GetNodes()[0].GetReference()
}

// link makes from block to.
func (f *fixture) link(ctx context.Context, from, to string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "spec-link-" + from + "-" + to,
		Project:   f.project,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_AddEdge{AddEdge: &daggerv1.AddEdgeOp{
			From: from, To: to, Kind: daggerv1.EdgeKind_EDGE_KIND_BLOCKS,
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}
