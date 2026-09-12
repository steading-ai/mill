package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ivy/mill/gen/harness/v1"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/gen/dagger/v1/daggerv1connect"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/cli"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workflow"
)

// The operator tools are specified against a real service over a real
// socket, with a ledger a spec composed through the same Connect API a
// worker writes it with. Nothing here fakes the store: what these specs are
// about is what a tool prints for a given ledger, and a fake ledger would
// make them specs about the fake.

// ledger is a store client on a running service's socket. The spec process
// is the account the service runs as, so it holds the service role and may
// write the record a worker would have written.
func ledger(m mill) millv1connect.StoreServiceClient {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", m.socket)
		},
	}}
	return millv1connect.NewStoreServiceClient(client, "http://mill")
}

// runs is a ledger with one run in it, at whatever point a spec needs.
type runs struct {
	mill     mill
	store    millv1connect.StoreServiceClient
	workflow *millv1.Workflow
	lease    string
	run      *millv1.Run
	nodeRef  string
}

// openRun starts a service, pushes quick-fix, and opens a run on a node that
// follows it. The run is `running` with its workflow selected, which is the
// state every read tool is interesting about.
func openRun(ctx context.Context, nodeRef, nodeID string) *runs {
	GinkgoHelper()
	return openRunOn(ctx, serving(ctx), nodeRef, nodeID)
}

// openRunOn is openRun against a service the caller already started, which
// is how a spec puts a run in the ledger of a process that is also running a
// worker.
func openRunOn(ctx context.Context, m mill, nodeRef, nodeID string) *runs {
	GinkgoHelper()
	store := ledger(m)

	definition, skills, err := workflow.Load(quickFix)
	Expect(err).NotTo(HaveOccurred())
	for _, skill := range skills {
		_, err := store.PutSkill(ctx, connect.NewRequest(&millv1.PutSkillRequest{Skill: skill}))
		Expect(err).NotTo(HaveOccurred())
	}
	return openRunFollowing(ctx, m, definition, nodeRef, nodeID)
}

// openRunFollowing is openRunOn with the workflow the run follows given,
// for a spec that needs two runs on two definitions.
func openRunFollowing(ctx context.Context, m mill, definition *millv1.Workflow, nodeRef, nodeID string) *runs {
	GinkgoHelper()
	store := ledger(m)
	stored, err := store.PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{Workflow: definition}))
	Expect(err).NotTo(HaveOccurred())

	r := &runs{mill: m, store: store, workflow: stored.Msg.GetWorkflow(), lease: "01HZZZZZZZZZZZZZZZZZZZZZZ9", nodeRef: nodeRef}
	created, err := store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId: r.lease, NodeId: nodeID, NodeRef: nodeRef, ContextId: nodeID,
		Generation: 1, WorkerRef: "spec@host", Branch: "mill/" + strings.ReplaceAll(nodeRef, "#", "-"),
	}))
	Expect(err).NotTo(HaveOccurred())
	r.run = created.Msg.GetRun()

	// Selection is a closed step run carrying the workflow id: the only
	// write that moves a run out of `selecting`.
	r.close(ctx, r.start(ctx, "select", millv1.StepKind_STEP_KIND_SKILL), &millv1.CloseStepRunRequest{
		WorkflowId: r.workflow.GetId(),
		Evidence: &millv1.Evidence{
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
			Value:      &millv1.Evidence_Structured{Structured: structOf(map[string]any{"workflow": "quick-fix"})},
			VerifiedBy: "spec",
		},
	})
	// The toolchain step readied the machine and closed on its record, as
	// every run's first step does before a session runs (docs/sandbox.md
	// §"Preparatory steps").
	r.close(ctx, r.start(ctx, "toolchain", millv1.StepKind_STEP_KIND_PREPARATORY), &millv1.CloseStepRunRequest{
		Evidence: &millv1.Evidence{
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
			Value:      &millv1.Evidence_Exit{Exit: &harnessv1.ProcessExit{Status: 0}},
			Output:     "mise all tools are installed\n",
			VerifiedBy: "preparatory toolchain",
		},
	})
	return r
}

// start opens an attempt at a step.
func (r *runs) start(ctx context.Context, step string, kind millv1.StepKind) *millv1.StepRun {
	GinkgoHelper()
	started, err := r.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
		LeaseId: r.lease, RunId: r.run.GetId(), StepName: step, Kind: kind, WorkerRef: "spec@host",
	}))
	Expect(err).NotTo(HaveOccurred())
	return started.Msg.GetStepRun()
}

// close records the evidence that closes an attempt.
func (r *runs) close(ctx context.Context, sr *millv1.StepRun, request *millv1.CloseStepRunRequest) {
	GinkgoHelper()
	request.LeaseId = r.lease
	request.StepRunId = sr.GetId()
	request.Evidence.StepRunId = sr.GetId()
	_, err := r.store.CloseStepRun(ctx, connect.NewRequest(request))
	Expect(err).NotTo(HaveOccurred())
}

// closeGatherContext closes quick-fix's first step on an artifact digest,
// which is how a run reaches the step the read tools are interesting about.
func (r *runs) closeGatherContext(ctx context.Context, digest string) {
	GinkgoHelper()
	r.close(ctx, r.start(ctx, "gather-context", millv1.StepKind_STEP_KIND_SKILL), &millv1.CloseStepRunRequest{
		Evidence: &millv1.Evidence{
			Kind:       millv1.EvidenceKind_EVIDENCE_KIND_ARTIFACT,
			Value:      &millv1.Evidence_Artifact{Artifact: digest},
			VerifiedBy: "mill: dagger accepted the artifact",
		},
	})
}

// fail records an attempt that ended badly, which is what a review round
// that found something looks like in the ledger.
func (r *runs) fail(ctx context.Context, step string, status int32) {
	GinkgoHelper()
	sr := r.start(ctx, step, millv1.StepKind_STEP_KIND_SKILL)
	_, err := r.store.FailStepRun(ctx, connect.NewRequest(&millv1.FailStepRunRequest{
		LeaseId: r.lease, StepRunId: sr.GetId(),
		Exit: processExit(status),
	}))
	Expect(err).NotTo(HaveOccurred())
}

// park trips the circuit breaker on the run, recording the gate a human is
// being asked to answer.
func (r *runs) park(ctx context.Context, gateNodeID string) {
	GinkgoHelper()
	_, err := r.store.Park(ctx, connect.NewRequest(&millv1.ParkRequest{
		LeaseId: r.lease, RunId: r.run.GetId(), GateNodeId: gateNodeID,
		Reason: millv1.ParkedReason_PARKED_REASON_RETRIES,
	}))
	Expect(err).NotTo(HaveOccurred())
}

// wait records the gate a run filed and is blocked on.
func (r *runs) wait(ctx context.Context, gateNodeID string, sr *millv1.StepRun) {
	GinkgoHelper()
	_, err := r.store.MarkWaiting(ctx, connect.NewRequest(&millv1.MarkWaitingRequest{
		LeaseId: r.lease, RunId: r.run.GetId(), GateNodeId: gateNodeID, StepRunId: sr.GetId(),
	}))
	Expect(err).NotTo(HaveOccurred())
}

// reload reads the run back.
func (r *runs) reload(ctx context.Context) *millv1.Run {
	GinkgoHelper()
	res, err := r.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: r.run.GetId()}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetSummary().GetRun()
}

// cli runs one command against this spec's service and returns its output.
func (r *runs) cli(ctx context.Context, args ...string) string {
	return r.mill.run(ctx, args...)
}

// gateFixture is a dagger with a gate node in it, for the tools that resolve
// a gate node id to a reference and a title.
type gateFixture struct {
	service *testsupport.Dagger
	graph   daggerv1connect.GraphServiceClient
	nodeRef string
	nodeID  string
	gateRef string
	gateID  string
	title   string
}

// daggerNode brings up a dagger with one project and one node in it: the
// work a run is for, and the node a gate will hang off.
func daggerNode(ctx context.Context) *gateFixture {
	GinkgoHelper()
	service := pg.StartDagger(ctx)
	graph := daggerv1connect.NewGraphServiceClient(service.HTTPClient(), service.BaseURL())
	_, err := graph.CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{
		RequestId: "cli-spec-project", Title: "mill cli specs", Slug: daggerProject,
	}))
	Expect(err).NotTo(HaveOccurred())

	created, err := graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "cli-spec-node", Project: daggerProject,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CreateNode{CreateNode: &daggerv1.CreateNodeOp{
			Title: "the work the run is for", Body: "a node with a gate on it",
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	f := &gateFixture{service: service, graph: graph, nodeRef: created.Msg.GetNodes()[0].GetReference()}
	f.nodeID = f.idOf(ctx, f.nodeRef)
	return f
}

// idOf is the node's ULID, which the ledger records and a patch response
// does not carry.
func (g *gateFixture) idOf(ctx context.Context, ref string) string {
	GinkgoHelper()
	res, err := g.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: daggerProject, Node: ref, View: daggerv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetNode().GetId()
}

// fileGate files the gate a run is blocked on: the same patch mill's own
// CreateGate sends — a user-assignee child of the node, with a blocks edge
// into it, labelled with the run and the step.
func (g *gateFixture) fileGate(ctx context.Context, runID, step, title string) {
	GinkgoHelper()
	filed, err := g.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "cli-spec-gate-" + step, Project: daggerProject,
		Ops: []*daggerv1.PatchOp{
			{Op: &daggerv1.PatchOp_CreateNode{CreateNode: &daggerv1.CreateNodeOp{
				Alias:        "gate",
				Title:        title,
				Body:         "the ask a person answers",
				Parent:       g.nodeRef,
				Kind:         "chore",
				AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER,
				Labels:       []string{"mill-gate", "mill-run-" + runID, "mill-step-" + step},
			}}},
			{Op: &daggerv1.PatchOp_AddEdge{AddEdge: &daggerv1.AddEdgeOp{
				From: "gate", To: g.nodeRef, Kind: daggerv1.EdgeKind_EDGE_KIND_BLOCKS,
			}}},
		},
	}))
	Expect(err).NotTo(HaveOccurred())
	g.gateRef = filed.Msg.GetNodes()[0].GetReference()
	g.gateID = g.idOf(ctx, g.gateRef)
	g.title = title
}

// daggerProject is the project the CLI specs file their nodes in.
const daggerProject = "millcli"

// structOf builds a struct value for evidence.
func structOf(fields map[string]any) *structpb.Struct {
	GinkgoHelper()
	value, err := structpb.NewStruct(fields)
	Expect(err).NotTo(HaveOccurred())
	return value
}

// processExit is how a failed attempt's process ended.
func processExit(status int32) *harnessv1.ProcessExit {
	return &harnessv1.ProcessExit{Status: status}
}

// connectRequest is a Connect request, spelled once so a spec that makes one
// directly reads like the tools do.
func connectRequest[T any](message *T) *connect.Request[T] {
	return connect.NewRequest(message)
}

// lines splits output into its non-empty lines.
func lines(output string) []string {
	var out []string
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// rowFor is the table row whose first cell is name.
func rowFor(output, name string) string {
	for _, line := range lines(output) {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return line
		}
	}
	return ""
}

// runCLI runs one command and returns everything a shell would have seen —
// what it printed and what it failed with — for the specs about refusals.
// The command tree silences its own errors, because cli.Run is what prints
// them; this puts the two back together.
func runCLI(ctx context.Context, socket string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	root := cli.NewRootCommand(&stdout, &stderr)
	root.SetArgs(append(args, "--socket", socket))
	err := root.ExecuteContext(ctx)
	out := stdout.String() + stderr.String()
	if err != nil {
		out += "mill: " + err.Error() + "\n"
	}
	return out, err
}

// nodeID is a ULID-shaped id for a node the ledger records but no dagger
// holds, for the specs that need no dagger at all.
func nodeID(n int) string {
	return fmt.Sprintf("01HZZZZZZZZZZZZZZZZZZZZZ%02d", n)
}
