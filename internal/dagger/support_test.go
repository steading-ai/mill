package dagger_test

import (
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"

	"github.com/steading-ai/mill/internal/dagger"
	"github.com/steading-ai/mill/internal/testsupport"
)

// fixture is a running dagger with a project in it, mill's client, and the
// generated client underneath. Two clients on purpose: the point of several
// specs is that mill's refusals are mill's own, which is only demonstrable
// beside a client that does not make them.
type fixture struct {
	service *testsupport.Dagger
	project string
	mill    *dagger.Client
	graph   skiftv1connect.GraphServiceClient
	work    skiftv1connect.WorkServiceClient
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
		graph: skiftv1connect.NewGraphServiceClient(client, service.BaseURL()),
		work:  skiftv1connect.NewWorkServiceClient(client, service.BaseURL()),
	}
	_, err := f.graph.CreateProject(ctx, connect.NewRequest(&skiftv1.CreateProjectRequest{
		RequestId: "mill-spec-project",
		Title:     "mill specs",
		Slug:      projectSlug,
	}))
	Expect(err).NotTo(HaveOccurred())
	return f
}

// node creates a node in the project and returns its reference.
func (f *fixture) node(ctx context.Context, title, body string, assignee skiftv1.AssigneeKind) string {
	GinkgoHelper()
	return f.nodeIn(ctx, f.project, title, body, assignee)
}

// nodeIn creates a node in project and returns its reference — node's own
// logic, parameterized, for the specs proving a claim or a read reaches
// across projects rather than being confined to the fixture's default one.
func (f *fixture) nodeIn(ctx context.Context, project, title, body string, assignee skiftv1.AssigneeKind) string {
	GinkgoHelper()
	res, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-" + project + "-" + title,
		Project:   project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CreateNode{CreateNode: &skiftv1.CreateNodeOp{
			Title:        title,
			Body:         body,
			AssigneeKind: assignee,
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	Expect(res.Msg.GetNodes()).To(HaveLen(1))
	return res.Msg.GetNodes()[0].GetReference()
}

// secondProjectSlug names the project secondProject creates: a second
// project beside the fixture's own, for specs proving a claim or a read
// reaches across projects.
const secondProjectSlug = "millspec-second"

// secondProject creates a second project beside the fixture's own and
// returns its slug.
func (f *fixture) secondProject(ctx context.Context) string {
	GinkgoHelper()
	_, err := f.graph.CreateProject(ctx, connect.NewRequest(&skiftv1.CreateProjectRequest{
		RequestId: "mill-spec-second-project",
		Title:     "mill specs (second)",
		Slug:      secondProjectSlug,
	}))
	Expect(err).NotTo(HaveOccurred())
	return secondProjectSlug
}

// unscoped is a client pinned to no project at all — a worker's own client
// when it is configured with no --project, claiming across every project
// its principal may read.
func (f *fixture) unscoped() *dagger.Client {
	GinkgoHelper()
	return dagger.New(f.service.HTTPClient(), f.service.BaseURL(), "",
		dagger.WithAgentRef("spec@host"), dagger.WithSessionRef("spec"))
}

// link makes from block to.
func (f *fixture) link(ctx context.Context, from, to string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-link-" + from + "-" + to,
		Project:   f.project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_AddEdge{AddEdge: &skiftv1.AddEdgeOp{
			From: from, To: to, Kind: skiftv1.EdgeKind_EDGE_KIND_BLOCKS,
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}
