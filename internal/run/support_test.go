package run_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/gen/dagger/v1/daggerv1connect"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/server"
	"github.com/ivy/mill/internal/testsupport"
	"github.com/ivy/mill/internal/workflow"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
)

// The run loop is specified end to end, because that is what it is: the only
// thing it does that no other package does is put dagger, the ledger, the
// workspace, a harness and a forge in one order. A fixture that faked any of
// the first three would specify the fake.
//
// Real: dagger, the ledger over PostgreSQL, git, the workspace. Scripted:
// the harness, because a spec that called a model would be a spec about the
// model; and the forge, because there is no GitLab on this host and the
// forge adapter has specs of its own (mill#13).

const project = "millspec"

// workflowDir is the quick-fix definition this repository seeds the store
// with. The spec pushes the real thing rather than a fixture, because "with
// quick-fix as the project default" is the criterion.
const workflowDir = "../../workflows/quick-fix"

// fixture is one spec's whole world.
type fixture struct {
	dagger    *testsupport.Dagger
	client    *dagger.Client
	graph     daggerv1connect.GraphServiceClient
	store     millv1connect.StoreServiceClient
	workflow  *millv1.Workflow
	harness   *fake.Harness
	forge     *fixtureForge
	origin    string
	root      string
	worker    string
	nodeRef   string
	nodeTitle string
}

// start builds the world: a dagger with a project in it, the ledger behind
// its own Connect handlers, the quick-fix workflow pushed, and a repository
// to clone.
func start(ctx context.Context) *fixture {
	GinkgoHelper()
	service := pg.StartDagger(ctx)
	httpClient := service.HTTPClient()

	f := &fixture{
		dagger: service,
		client: dagger.New(httpClient, service.BaseURL(), project, dagger.WithAgentRef("spec@host"), dagger.WithSessionRef("spec")),
		graph:  daggerv1connect.NewGraphServiceClient(httpClient, service.BaseURL()),
		forge:  &fixtureForge{},
		worker: "spec@host",
	}

	_, err := f.graph.CreateProject(ctx, connect.NewRequest(&daggerv1.CreateProjectRequest{
		RequestId: "mill-spec-project", Title: "mill specs", Slug: project,
	}))
	Expect(err).NotTo(HaveOccurred())

	srv := server.New(server.Config{Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))},
		server.NewStoreService(domain.New(pg.Database(ctx))))
	ts := httptest.NewServer(srv.Handler())
	DeferCleanup(ts.Close)
	f.store = millv1connect.NewStoreServiceClient(ts.Client(), ts.URL)

	f.pushWorkflow(ctx)
	f.root = GinkgoT().TempDir()
	f.origin = newOrigin(f.root)
	return f
}

// pushWorkflow stores the quick-fix definition and its skills.
func (f *fixture) pushWorkflow(ctx context.Context) {
	GinkgoHelper()
	definition, skills, err := workflow.Load(workflowDir)
	Expect(err).NotTo(HaveOccurred())
	for _, skill := range skills {
		_, err := f.store.PutSkill(ctx, connect.NewRequest(&millv1.PutSkillRequest{Skill: skill}))
		Expect(err).NotTo(HaveOccurred())
	}
	stored, err := f.store.PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{Workflow: definition}))
	Expect(err).NotTo(HaveOccurred())
	f.workflow = stored.Msg.GetWorkflow()
}

// node files the work the run is for and remembers its reference.
func (f *fixture) node(ctx context.Context, title, body string) string {
	GinkgoHelper()
	res, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "spec-node-" + title,
		Project:   project,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CreateNode{CreateNode: &daggerv1.CreateNodeOp{
			Title: title, Body: body,
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	f.nodeRef = res.Msg.GetNodes()[0].GetReference()
	f.nodeTitle = title
	return f.nodeRef
}

// input files a done predecessor whose result the node's prompt has to carry
// as data.
func (f *fixture) input(ctx context.Context, title, result string) string {
	GinkgoHelper()
	ref := f.node(ctx, title, "the predecessor's work order")
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "spec-done-" + title,
		Project:   project,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
			Node: ref, Result: &daggerv1.Result{Body: result},
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	return ref
}

// blocks makes from a blocking predecessor of to.
func (f *fixture) blocks(ctx context.Context, from, to string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "spec-edge-" + from + to,
		Project:   project,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_AddEdge{AddEdge: &daggerv1.AddEdgeOp{
			From: from, To: to, Kind: daggerv1.EdgeKind_EDGE_KIND_BLOCKS,
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// runner builds the loop under test.
func (f *fixture) runner(options ...func(*run.Options)) *run.Runner {
	GinkgoHelper()
	if f.harness == nil {
		f.script()
	}
	registry := harness.NewRegistry()
	Expect(registry.Register(run.DefaultHarness, f.harness)).To(Succeed())

	opts := run.Options{
		Store:       f.store,
		Dagger:      f.client,
		Workspaces:  f.workspaces(),
		Forges:      func(context.Context) (forge.Forge, error) { return f.forge, nil },
		Harnesses:   registry,
		WorkerRef:   f.worker,
		Workflow:    "quick-fix",
		WorkflowDir: mustAbs(workflowDir),
		Origin:      f.origin,
		Out:         GinkgoWriter,
		Log:         slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
	}
	for _, option := range options {
		option(&opts)
	}
	return run.New(opts)
}

// workspaces makes a local workspace under the spec's temporary directory,
// reattaching one an earlier invocation left behind.
func (f *fixture) workspaces() run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec) (run.Workspace, error) {
		ws := local.New(local.Options{Root: filepath.Join(f.root, "workspaces")})
		if _, err := ws.Attach(ctx, spec); err == nil {
			return ws, nil
		}
		if _, err := ws.Create(ctx, spec); err != nil {
			return nil, err
		}
		return ws, nil
	}
}

// runOf reads the ledger's record of the node's run.
func (f *fixture) runOf(ctx context.Context) *millv1.Run {
	GinkgoHelper()
	node, err := f.client.Node(ctx, f.nodeRef)
	Expect(err).NotTo(HaveOccurred())
	res, err := f.store.GetRunByNode(ctx, connect.NewRequest(&millv1.GetRunByNodeRequest{NodeId: node.GetId()}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetRun()
}

// inspect reads every attempt the ledger holds for the node's run.
func (f *fixture) inspect(ctx context.Context) *millv1.InspectResponse {
	GinkgoHelper()
	res, err := f.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{NodeRef: f.nodeRef}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg
}

// closedSteps is the set of step names with a closed attempt and evidence.
func closedSteps(detail *millv1.InspectResponse) map[string]millv1.EvidenceKind {
	closed := map[string]millv1.EvidenceKind{}
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED && d.GetEvidence() != nil {
			closed[d.GetStepRun().GetStepName()] = d.GetEvidence().GetKind()
		}
	}
	return closed
}

// script fixes what the harness replays, one entry per turn. The last is
// reused, so a spec that cares about one turn names one.
func (f *fixture) script(scripts ...fake.Script) {
	f.harness = fake.New(scripts...)
}

// quickFixScripts is one clean pass of quick-fix: four skill steps, each
// ending well, each billing something.
func quickFixScripts() []fake.Script {
	return []fake.Script{
		{Terminal: fake.Terminal("context gathered", nil, usage(10))},
		{Terminal: fake.Terminal("the change is made", nil, usage(20))},
		{Terminal: fake.Terminal("committed", nil, usage(5))},
		{Terminal: fake.Terminal("opened the pull request", nil, usage(5))},
	}
}

// usage is a scripted turn's token counts.
func usage(n int64) harness.Usage {
	return harness.Usage{"claude-sonnet": {Input: n, Output: n, CacheRead: 0, CacheWrite: 0}}
}

// fixtureForge is the project's forge, scripted. There is no GitLab on this
// host, and what the loop needs of a forge is that a branch has a pull
// request, that the pull request merges, and that the merge has a commit.
type fixtureForge struct {
	mu       sync.Mutex
	pr       forge.PullRequest
	merged   bool
	refuse   error
	checksAt forge.Status
}

func (f *fixtureForge) Push(_ context.Context, request forge.PushRequest) (string, error) {
	return "", nil
}

func (f *fixtureForge) OpenPR(_ context.Context, request forge.OpenPRRequest) (forge.PullRequest, error) {
	return f.PR(context.Background(), request.Branch)
}

func (f *fixtureForge) PR(_ context.Context, branch string) (forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pr.URL == "" {
		f.pr = forge.PullRequest{
			Number: 1, URL: "https://forge.test/mill/merge_requests/1",
			Branch: branch, Target: "main", HeadSHA: strings.Repeat("a", 40), State: "open",
		}
	}
	return f.pr, nil
}

func (f *fixtureForge) Checks(context.Context, string) (forge.Status, error) {
	if f.checksAt == "" {
		return forge.Success, nil
	}
	return f.checksAt, nil
}

func (f *fixtureForge) Merge(_ context.Context, pr forge.PullRequest) (forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return pr, f.refuse
	}
	f.merged = true
	f.pr.State = "merged"
	f.pr.MergeCommit = strings.Repeat("b", 40)
	return f.pr, nil
}

func (f *fixtureForge) MergeCommit(_ context.Context, pr forge.PullRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.merged {
		return "", forge.ErrNotMerged
	}
	return f.pr.MergeCommit, nil
}

var _ forge.Forge = (*fixtureForge)(nil)

// newOrigin makes a repository for the run's workspace to clone: one commit
// on the default branch, and nothing else.
func newOrigin(root string) string {
	GinkgoHelper()
	work := filepath.Join(root, "origin-work")
	Expect(os.MkdirAll(work, 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(work, "README.md"), []byte("as cloned\n"), 0o644)).To(Succeed())

	git(work, "init", "--initial-branch=main")
	git(work, "config", "user.email", "spec@example.test")
	git(work, "config", "user.name", "Spec")
	git(work, "config", "commit.gpgsign", "false")
	git(work, "add", "README.md")
	git(work, "commit", "-m", "first")

	bare := filepath.Join(root, "origin.git")
	git(root, "clone", "--bare", work, bare)
	return bare
}

// git runs one git command for the fixture, through the process chokepoint.
func git(dir string, args ...string) {
	GinkgoHelper()
	process, err := workspace.Spawner{}.Exec(context.Background(), workspace.Command{
		Path: "git", Args: args, Dir: dir,
		Env: map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"},
	})
	Expect(err).NotTo(HaveOccurred())
	stdout, stderr := readAll(process.Stdout), readAll(process.Stderr)
	result := process.Wait()
	Expect(result.Status).To(BeZero(), "git %s: %s %s", strings.Join(args, " "), stdout(), stderr())
}

// readAll drains a pipe in the background.
func readAll(reader interface{ Read([]byte) (int, error) }) func() string {
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	return func() string { return <-done }
}

// mustAbs is a path the check runner can resolve a command against.
func mustAbs(path string) string {
	GinkgoHelper()
	absolute, err := filepath.Abs(path)
	Expect(err).NotTo(HaveOccurred())
	return absolute
}

// result is the node's result once dagger says it is done.
func (f *fixture) result(ctx context.Context) (*daggerv1.Result, error) {
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: daggerv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetResult(), nil
}

// leaseOf is the live lease on the node, or nothing.
func (f *fixture) leaseOf(ctx context.Context) *daggerv1.Lease {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: daggerv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetLease()
}

// expire takes the node's lease away, which is what the run loop's next
// heartbeat has to notice. Dagger's own expiry is a timer; a release is the
// same end state reached at a time the spec chooses.
func (f *fixture) expire(ctx context.Context, leaseID string) {
	GinkgoHelper()
	Expect(f.client.Release(ctx, leaseID,
		daggerv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "spec: forcing expiry")).To(Succeed())
}

// reclaim is a second client taking the node, and returns its lease id.
func (f *fixture) reclaim(ctx context.Context) string {
	GinkgoHelper()
	other := dagger.New(f.dagger.HTTPClient(), f.dagger.BaseURL(), project,
		dagger.WithAgentRef("other@host"), dagger.WithSessionRef("other"))
	claimed, err := other.Claim(ctx, f.nodeRef)
	Expect(err).NotTo(HaveOccurred())
	return claimed.Lease.GetId()
}

// cancelRun asks the run's worker to stop at its next heartbeat.
func (f *fixture) cancelRun(ctx context.Context) error {
	_, err := f.store.Cancel(ctx, connect.NewRequest(&millv1.CancelRequest{
		RunId:  f.runOf(ctx).GetId(),
		Reason: "the spec is done with it",
	}))
	return err
}

// daggerUsage is what dagger recorded against the node's attempt, by model.
// Dagger sums the records of a lease, so posting the same step twice would
// double what this returns.
func (f *fixture) daggerUsage(ctx context.Context) map[string]int64 {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: daggerv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	totals := map[string]int64{}
	for _, actual := range res.Msg.GetUsage().GetActual() {
		totals[actual.GetModel()] += actual.GetInputTokens()
	}
	return totals
}

// ledgerUsage is what the ledger holds for the same run, by model.
func ledgerUsage(detail *millv1.InspectResponse) map[string]int64 {
	totals := map[string]int64{}
	for _, d := range detail.GetSteps() {
		for _, t := range d.GetTotals() {
			totals[t.GetModel()] += t.GetInputTokens()
		}
	}
	return totals
}

// artifactNames is what a result carries.
func artifactNames(result *daggerv1.Result) []string {
	names := make([]string, 0, len(result.GetArtifacts()))
	for _, artifact := range result.GetArtifacts() {
		names = append(names, artifact.GetName())
	}
	return names
}

// referenceOf is the URI of a result's named reference artifact.
func referenceOf(result *daggerv1.Result, name string) string {
	for _, artifact := range result.GetArtifacts() {
		if artifact.GetName() != name {
			continue
		}
		for _, part := range artifact.GetParts() {
			if ref := part.GetReference(); ref != nil {
				return ref.GetUri()
			}
		}
	}
	return ""
}

// patchBreaker sits between the run loop and dagger and refuses the next
// ApplyPatch once it is armed. It is how a spec stages the failure of the
// completing patch — the one write whose ordering against `store.Done` is
// the whole of Invariant 11 — without reaching inside the client.
type patchBreaker struct {
	socket string
	armed  atomic.Bool
	broke  atomic.Bool
}

func (p *patchBreaker) RoundTrip(req *http.Request) (*http.Response, error) {
	if p.armed.Load() && strings.HasSuffix(req.URL.Path, "/ApplyPatch") {
		p.armed.Store(false)
		p.broke.Store(true)
		return nil, errors.New("the patch never reached dagger")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p.socket)
		},
	}
	return transport.RoundTrip(req)
}

// breaking builds a dagger client whose patches this spec can interrupt.
func (f *fixture) breaking() (*dagger.Client, *patchBreaker) {
	breaker := &patchBreaker{socket: f.dagger.Socket}
	return dagger.New(&http.Client{Transport: breaker}, f.dagger.BaseURL(), project), breaker
}

// arming wraps the store client so that the completing patch's request id
// being minted is what arms the breaker: the very next dagger write is the
// patch that completes the node.
type arming struct {
	millv1connect.StoreServiceClient
	breaker *patchBreaker
}

func (a arming) SetCompleteRequestId(ctx context.Context, req *connect.Request[millv1.SetCompleteRequestIdRequest]) (*connect.Response[millv1.SetCompleteRequestIdResponse], error) {
	res, err := a.StoreServiceClient.SetCompleteRequestId(ctx, req)
	if err == nil {
		a.breaker.armed.Store(true)
	}
	return res, err
}

// fencing wraps the store client and answers one method exactly as the store
// answers a write whose lease is no longer the run's.
type fencing struct {
	millv1connect.StoreServiceClient
	calls atomic.Int64
}

func (f *fencing) CloseStepRun(ctx context.Context, req *connect.Request[millv1.CloseStepRunRequest]) (*connect.Response[millv1.CloseStepRunResponse], error) {
	f.calls.Add(1)
	return nil, connect.NewError(connect.CodeFailedPrecondition,
		errors.New("run: presented lease is not the run's current lease: write is fenced"))
}
