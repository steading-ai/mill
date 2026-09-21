package server_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"time"

	"log/slog"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/server"
	"github.com/ivy/mill/internal/server/interceptor"
)

// The uids the specs' role map names. They are invented rather than read
// from the account running the specs, so what a spec asserts about a role is
// about the map and not about who happens to be running it.
const (
	serviceUID  = 9000
	operatorUID = 9001
	readerUID   = 9002
	strangerUID = 9003
)

// lease is the dagger lease the fixture's worker holds. Every fenced write
// presents it.
const lease = "lease-1"

// sha is a git object name, which is what commit evidence has to be.
const sha = "0123456789abcdef0123456789abcdef01234567"

// service is the handlers over one spec's own database, reachable as any
// uid: each uid gets an HTTP server whose connections carry that uid's
// credentials, which is what the role interceptor reads. The transport is
// TCP rather than the unix socket only because a spec cannot connect under
// a uid it does not have; [Describe("The unix socket")] covers the socket
// itself.
type service struct {
	pool    *pgxpool.Pool
	handler http.Handler
	clients map[int]millv1connect.StoreServiceClient
	// logDir is where this service's Logs handler reads activity records
	// from, empty unless a spec populates it (internal/testsupport's
	// AppendActivity, at run.ActivityPath under this directory).
	logDir string
}

// newService clones a migrated database and mounts the handlers over it,
// behind the role interceptor and the specs' role map, with the observer's
// default fallback. Its Logs handler reads from a fresh temp directory,
// empty unless a spec populates it.
func newService(ctx context.Context) *service {
	return newServiceFallback(ctx, server.DefaultObserverFallback)
}

// newServiceFallback is newService with the observer's fallback given
// rather than defaulted — what a spec proving a followed stream woke on
// signal, not the fallback timer, needs: a fallback long enough that the
// spec's own close-and-signal always beats it, but short enough that the
// spec does not sit through the default 5s if that proof is ever wrong.
func newServiceFallback(ctx context.Context, fallback time.Duration) *service {
	GinkgoHelper()
	return newServiceWith(ctx, fallback)
}

// newServiceWithStore is newService over a StoreService built with opts, for
// the specs that need PutWorkflow's collision check configured with a
// worker's own mcp-servers names.
func newServiceWithStore(ctx context.Context, opts ...server.StoreServiceOption) *service {
	GinkgoHelper()
	return newServiceWith(ctx, server.DefaultObserverFallback, opts...)
}

// newServiceWith builds the service every other constructor here narrows:
// the observer's fallback and the store's options both given.
func newServiceWith(ctx context.Context, fallback time.Duration, opts ...server.StoreServiceOption) *service {
	GinkgoHelper()
	pool := pg.Database(ctx)
	roles := interceptor.RoleMap{
		ServiceUID: serviceUID,
		Roles: map[int]interceptor.Role{
			operatorUID: interceptor.RoleOperator,
			readerUID:   interceptor.RoleReader,
		},
	}
	logDir := GinkgoT().TempDir()
	srv := server.New(server.Config{
		Logger:       specLogger(),
		Interceptors: []connect.Interceptor{interceptor.NewRoles(roles, server.Requirement())},
	}, server.NewStoreService(domain.New(pool), server.NewObserver(fallback), logDir, opts...), nil)
	return &service{pool: pool, handler: srv.Handler(), clients: map[int]millv1connect.StoreServiceClient{}, logDir: logDir}
}

// specLogger sends the server's log to the spec's writer, where Ginkgo
// shows it only for failures or under -v.
func specLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(GinkgoWriter, nil))
}

// as returns a client whose connections present uid.
func (s *service) as(uid int) millv1connect.StoreServiceClient {
	GinkgoHelper()
	if client, ok := s.clients[uid]; ok {
		return client
	}
	ts := httptest.NewUnstartedServer(s.handler)
	ts.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return interceptor.WithPeerCredentials(ctx, interceptor.PeerCredentials{UID: uid, GID: uid, PID: 1})
	}
	ts.Start()
	DeferCleanup(ts.Close)
	client := millv1connect.NewStoreServiceClient(ts.Client(), ts.URL)
	s.clients[uid] = client
	return client
}

// client is the service's own uid: the worker face of the binary, which is
// the caller of everything that writes.
func (s *service) client() millv1connect.StoreServiceClient { return s.as(serviceUID) }

// evidenceRows is how many evidence rows the ledger holds, read straight
// from the table for the specs that must see the row rather than what a
// handler returned.
func (s *service) evidenceRows(ctx context.Context) int {
	GinkgoHelper()
	var count int
	Expect(s.pool.QueryRow(ctx, `SELECT count(*) FROM evidence`).Scan(&count)).To(Succeed())
	return count
}

// fixture is a run in the state a worker resumes into: a stored workflow,
// a run whose selection step has closed, and therefore a run that is
// running under [lease].
type fixture struct {
	*service
	workflow *millv1.Workflow
	run      *millv1.Run
}

// steps is the workflow every fixture follows. Each step names the one
// evidence kind that closes it, which is what CloseStepRun is judged
// against.
var steps = []*millv1.Step{
	{Name: "implement", Kind: millv1.StepKind_STEP_KIND_SKILL, Evidence: millv1.EvidenceKind_EVIDENCE_KIND_COMMIT},
	{Name: "review", Kind: millv1.StepKind_STEP_KIND_SKILL, Evidence: millv1.EvidenceKind_EVIDENCE_KIND_VERDICT},
	{Name: "approve", Kind: millv1.StepKind_STEP_KIND_GATE, Evidence: millv1.EvidenceKind_EVIDENCE_KIND_GATE},
}

// newFixture stores the workflow, creates a run and closes its selection
// step, which is what moves a run from selecting to running.
func newFixture(ctx context.Context, nodeRef string) *fixture {
	GinkgoHelper()
	return newFixtureOn(ctx, newService(ctx), nodeRef)
}

// newFixtureFallback is newFixture over a service built with fallback
// rather than the observer's default — see newServiceFallback.
func newFixtureFallback(ctx context.Context, nodeRef string, fallback time.Duration) *fixture {
	GinkgoHelper()
	return newFixtureOn(ctx, newServiceFallback(ctx, fallback), nodeRef)
}

// newFixtureOn is newFixture over a service newFixture or newFixtureFallback
// already built: store the workflow, create a run and close its selection
// step, which is what moves a run from selecting to running.
func newFixtureOn(ctx context.Context, svc *service, nodeRef string) *fixture {
	GinkgoHelper()
	f := &fixture{service: svc}
	client := f.client()

	stored, err := client.PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{
		Workflow: &millv1.Workflow{Name: "feature", Scope: millv1.Scope_SCOPE_ORG, Steps: steps},
	}))
	Expect(err).NotTo(HaveOccurred())
	f.workflow = stored.Msg.GetWorkflow()

	created, err := client.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId:   lease,
		NodeId:    "node-" + nodeRef,
		NodeRef:   nodeRef,
		ContextId: "ctx-" + nodeRef,
		WorkerRef: "worker-1",
		Branch:    "mill/" + nodeRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	f.run = created.Msg.GetRun()

	assess := f.start(ctx, "assess", millv1.StepKind_STEP_KIND_SKILL)
	closed, err := client.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
		LeaseId:    lease,
		StepRunId:  assess.GetId(),
		Evidence:   structuredEvidence(),
		WorkflowId: f.workflow.GetId(),
		Tier:       millv1.Tier_TIER_FEATURE,
	}))
	Expect(err).NotTo(HaveOccurred())
	Expect(closed.Msg.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
	f.run = f.reload(ctx)
	Expect(f.run.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
	return f
}

// anotherRun creates a second run on this fixture's own service, under the
// workflow newFixture already stored — which is what an every-run spec
// needs to see more than one run without paying for a second database.
func (f *fixture) anotherRun(ctx context.Context, nodeRef string) *fixture {
	GinkgoHelper()
	client := f.client()
	created, err := client.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId:   lease,
		NodeId:    "node-" + nodeRef,
		NodeRef:   nodeRef,
		ContextId: "ctx-" + nodeRef,
		WorkerRef: "worker-1",
		Branch:    "mill/" + nodeRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	g := &fixture{service: f.service, workflow: f.workflow, run: created.Msg.GetRun()}

	assess := g.start(ctx, "assess", millv1.StepKind_STEP_KIND_SKILL)
	closed, err := client.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
		LeaseId:    lease,
		StepRunId:  assess.GetId(),
		Evidence:   structuredEvidence(),
		WorkflowId: g.workflow.GetId(),
		Tier:       millv1.Tier_TIER_FEATURE,
	}))
	Expect(err).NotTo(HaveOccurred())
	Expect(closed.Msg.GetStepRun().GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
	g.run = g.reload(ctx)
	Expect(g.run.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
	return g
}

// start opens an attempt at a step.
func (f *fixture) start(ctx context.Context, stepName string, kind millv1.StepKind) *millv1.StepRun {
	GinkgoHelper()
	started, err := f.client().StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
		LeaseId:   lease,
		RunId:     f.run.GetId(),
		StepName:  stepName,
		Kind:      kind,
		WorkerRef: "worker-1",
	}))
	Expect(err).NotTo(HaveOccurred())
	return started.Msg.GetStepRun()
}

// reload reads the run back through the wire.
func (f *fixture) reload(ctx context.Context) *millv1.Run {
	GinkgoHelper()
	got, err := f.client().GetRunByNode(ctx, connect.NewRequest(&millv1.GetRunByNodeRequest{
		NodeId: f.run.GetNodeId(),
	}))
	Expect(err).NotTo(HaveOccurred())
	return got.Msg.GetRun()
}

// wait files a gate on the run and marks it waiting, which is the state a
// run reached through a gate step is in.
func (f *fixture) wait(ctx context.Context) *millv1.Run {
	GinkgoHelper()
	stepRun := f.start(ctx, "approve", millv1.StepKind_STEP_KIND_GATE)
	waiting, err := f.client().MarkWaiting(ctx, connect.NewRequest(&millv1.MarkWaitingRequest{
		LeaseId:    lease,
		RunId:      f.run.GetId(),
		GateNodeId: "gate-node",
		StepRunId:  stepRun.GetId(),
	}))
	Expect(err).NotTo(HaveOccurred())
	return waiting.Msg.GetRun()
}

// commitEvidence is what closes the implement step.
func commitEvidence() *millv1.Evidence {
	return &millv1.Evidence{
		Kind:       millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
		Value:      &millv1.Evidence_Commit{Commit: sha},
		VerifiedBy: "git-cat-file",
	}
}

// structuredEvidence is what closes the selection step.
func structuredEvidence() *millv1.Evidence {
	GinkgoHelper()
	document, err := structpb.NewStruct(map[string]any{"workflow": "feature", "tier": "feature"})
	Expect(err).NotTo(HaveOccurred())
	return &millv1.Evidence{
		Kind:       millv1.EvidenceKind_EVIDENCE_KIND_STRUCTURED,
		Value:      &millv1.Evidence_Structured{Structured: document},
		VerifiedBy: "assessment",
	}
}

// modelTotal finds totals's entry for model, or fails the spec: every
// assertion that wants one model's ModelTotals names the model it expects to
// find, so a missing entry is a spec bug, not a nil to check for.
func modelTotal(totals []*millv1.ModelTotals, model string) *millv1.ModelTotals {
	GinkgoHelper()
	for _, t := range totals {
		if t.GetModel() == model {
			return t
		}
	}
	Fail(fmt.Sprintf("no ModelTotals for model %q among %d entries", model, len(totals)))
	return nil
}
