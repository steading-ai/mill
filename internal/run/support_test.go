package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

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
	comments []string
	deleted  []string
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

func (f *fixtureForge) ClosePR(_ context.Context, pr forge.PullRequest, comment string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pr.State = "closed"
	f.comments = append(f.comments, comment)
	return nil
}

func (f *fixtureForge) DeleteBranch(_ context.Context, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, branch)
	return nil
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

// variant is the quick-fix definition under a new name with the changes a
// spec needs: a gate policy that does not hold, a review step that always
// fails. Pushing a mutated definition rather than writing a fixture
// workflow keeps the specs against the real steps, capabilities and skills
// the repository ships.
func (f *fixture) variant(ctx context.Context, name string, mutate func(*millv1.Workflow)) string {
	GinkgoHelper()
	definition := proto.Clone(f.workflow).(*millv1.Workflow)
	definition.Id = ""
	definition.Name = name
	mutate(definition)
	version, err := workflow.Version(definition)
	Expect(err).NotTo(HaveOccurred())
	definition.Version = version
	stored, err := f.store.PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{Workflow: definition}))
	Expect(err).NotTo(HaveOccurred())
	Expect(stored.Msg.GetWorkflow().GetName()).To(Equal(name))
	return name
}

// stepOf finds a step in a definition so a spec can change it in place.
func stepOf(definition *millv1.Workflow, name string) *millv1.Step {
	GinkgoHelper()
	for _, step := range definition.GetSteps() {
		if step.GetName() == name {
			return step
		}
	}
	Fail("the workflow has no step " + name)
	return nil
}

// insertAfter puts a step into a definition immediately after a named one.
func insertAfter(definition *millv1.Workflow, after string, step *millv1.Step) {
	GinkgoHelper()
	steps := make([]*millv1.Step, 0, len(definition.GetSteps())+1)
	for _, existing := range definition.GetSteps() {
		steps = append(steps, existing)
		if existing.GetName() == after {
			steps = append(steps, step)
		}
	}
	Expect(steps).To(HaveLen(len(definition.GetSteps()) + 1))
	definition.Steps = steps
}

// reviewStep is a skill step that reviews what implement did and sends the
// work back when it does not like it. quick-fix has none — a quick fix that
// needs a review is the wrong tier — so the specs about revise and the
// circuit breaker add one.
func reviewStep(skill *millv1.SkillRef) *millv1.Step {
	return &millv1.Step{
		Name:         "review",
		Kind:         millv1.StepKind_STEP_KIND_SKILL,
		Session:      "review",
		Skill:        skill,
		Evidence:     millv1.EvidenceKind_EVIDENCE_KIND_EXIT,
		Capabilities: []millv1.Capability{millv1.Capability_CAPABILITY_READ},
		OnFailure: &millv1.OnFailure{
			Kind:         millv1.OnFailureKind_ON_FAILURE_KIND_REVISE,
			ReviseTarget: "implement",
		},
		Retries: 3,
		Timeout: durationpb.New(time.Minute),
	}
}

// failed is a session that ran, said what it found, and ended badly. Its
// final message is what the mill hands the revise target as feedback.
func failed(final string) fake.Script {
	return fake.Script{Terminal: &harness.Terminal{
		FinalMessage: final,
		Exit:         harness.ProcessExit{Status: 1},
		Usage:        usage(1),
	}}
}

// ok is a session that ran and finished.
func ok(final string) fake.Script {
	return fake.Script{Terminal: fake.Terminal(final, nil, usage(1))}
}

// gatesOn is every user-assigned child of the node: the gates, and nothing
// else, because nothing else in these specs is assigned to a person.
func (f *fixture) gatesOn(ctx context.Context) []*daggerv1.Node {
	GinkgoHelper()
	return f.gatesOnNode(ctx, f.nodeRef)
}

// theGate is the node's one gate, and fails the spec when there is not
// exactly one.
func (f *fixture) theGate(ctx context.Context) *daggerv1.Node {
	GinkgoHelper()
	gates := f.gatesOn(ctx)
	Expect(gates).To(HaveLen(1), "exactly one gate is filed")
	return gates[0]
}

// comments are what the node carries, oldest first.
func (f *fixture) comments(ctx context.Context) []string {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: daggerv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	bodies := make([]string, 0, len(res.Msg.GetComments()))
	for _, comment := range res.Msg.GetComments() {
		bodies = append(bodies, comment.GetBody())
	}
	return bodies
}

// approveGate is a person answering a gate out of band, by completing it —
// the only answer that satisfies the edge and lets the node be claimed
// again. It writes as a human does: no lease, and no mill request id.
//
// A denial is the other half of the same story and is not here, because a
// person who denies drops the gate, and dagger never lets the node it blocks
// become ready again. The run that records a denial is therefore the one
// still holding the lease, which is single-run mode; see the specs that put
// an [answerer] at the terminal.
func (f *fixture) approveGate(ctx context.Context, ref string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: "spec-answer-" + ref, Project: project,
		Ops: []*daggerv1.PatchOp{{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
			Node: ref, Result: &daggerv1.Result{Body: "the spec says yes"},
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// stepRuns is every attempt at a named step, oldest first.
func stepRuns(detail *millv1.InspectResponse, name string) []*millv1.StepRun {
	var runs []*millv1.StepRun
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetStepName() == name {
			runs = append(runs, d.GetStepRun())
		}
	}
	return runs
}

// sessionsOf is the session id each attempt at a step ran in.
func sessionsOf(detail *millv1.InspectResponse, name string) []string {
	var ids []string
	for _, sr := range stepRuns(detail, name) {
		ids = append(ids, sr.GetSessionId())
	}
	return ids
}

// failingStore refuses one named write, which is how a spec stages a
// process that died between two writes of an ordered sequence.
type failingStore struct {
	millv1connect.StoreServiceClient
	calls atomic.Int64
}

func (f *failingStore) MarkWaiting(context.Context, *connect.Request[millv1.MarkWaitingRequest]) (*connect.Response[millv1.MarkWaitingResponse], error) {
	f.calls.Add(1)
	return nil, errors.New("the process died before the store write")
}

// releases records every lease release the run made, with its reason, by
// reading the requests on their way to dagger. The reason is part of the
// contract — a gate, a budget stop and a preemption are three different
// things to whoever reads dagger's leases afterwards — and nothing dagger
// returns afterwards says which one was sent.
type releases struct {
	socket string
	mu     sync.Mutex
	seen   []daggerv1.ReleaseReason
}

func (r *releases) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/Release") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		var release daggerv1.ReleaseRequest
		if err := proto.Unmarshal(body, &release); err == nil {
			r.mu.Lock()
			r.seen = append(r.seen, release.GetReason())
			r.mu.Unlock()
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
		},
	}
	return transport.RoundTrip(req)
}

// reasons is what has been released so far.
func (r *releases) reasons() []daggerv1.ReleaseReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]daggerv1.ReleaseReason{}, r.seen...)
}

// watching builds a dagger client whose releases this spec can read.
func (f *fixture) watching() (*dagger.Client, *releases) {
	watcher := &releases{socket: f.dagger.Socket}
	return dagger.New(&http.Client{Transport: watcher}, f.dagger.BaseURL(), project,
		dagger.WithAgentRef(f.worker), dagger.WithSessionRef("spec")), watcher
}

// reviewer is a harness whose review sessions always find something and
// whose every other session finishes cleanly.
//
// The scripted harness replays a fixed list, and a revise loop's turns
// interleave by a rule the spec would have to predict; routing on the
// session's model instead makes the spec say what it means — "the review
// fails every time" — and leaves the ordering to the loop under test, which
// is the thing being specified.
type reviewer struct {
	// Model is the model the review step declares, which is how a turn in
	// that step is told from the rest.
	model string

	mu       sync.Mutex
	rounds   int
	sessions []string
	verbs    []string
}

// Start begins a turn.
func (r *reviewer) Start(_ context.Context, session harness.Session, _ harness.Prompt, _ []harness.Attachment, _ harness.Policy) (*harness.Turn, error) {
	return r.play("start", session), nil
}

// Resume continues one.
func (r *reviewer) Resume(_ context.Context, session harness.Session, _ harness.Prompt, _ []harness.Attachment, _ harness.Policy) (*harness.Turn, error) {
	return r.play("resume", session), nil
}

// Cancel ends a turn that is not running, which is not an error.
func (r *reviewer) Cancel(context.Context, harness.Session) error { return nil }

func (r *reviewer) play(verb string, session harness.Session) *harness.Turn {
	r.mu.Lock()
	r.sessions = append(r.sessions, session.ID)
	r.verbs = append(r.verbs, verb)
	reviewing := session.Model == r.model
	if reviewing {
		r.rounds++
	}
	round := r.rounds
	r.mu.Unlock()

	turn := harness.NewTurn()
	go func() {
		if reviewing {
			turn.Finish(&harness.Terminal{
				FinalMessage: fmt.Sprintf("round %d: the change still does the wrong thing", round),
				Exit:         harness.ProcessExit{Status: 1},
				Usage:        usage(1),
			}, nil)
			return
		}
		turn.Finish(fake.Terminal("done", nil, usage(1)), nil)
	}()
	return turn
}

var _ harness.Harness = (*reviewer)(nil)

// withHarness runs the loop against an adapter of the spec's own, in place
// of the scripted one.
func withHarness(adapter harness.Harness) func(*run.Options) {
	return func(o *run.Options) {
		registry := harness.NewRegistry()
		Expect(registry.Register(run.DefaultHarness, adapter)).To(Succeed())
		o.Harnesses = registry
	}
}

// reviewedFix is quick-fix with a review step after the check, which is what
// the revise and circuit-breaker specs need: a step that fails, sends the
// work back to implement, and eventually runs out of attempts.
func (f *fixture) reviewedFix(ctx context.Context, model string) string {
	GinkgoHelper()
	return f.variant(ctx, "reviewed-fix", func(definition *millv1.Workflow) {
		review := reviewStep(stepOf(definition, "implement").GetSkill())
		review.Harness = map[string]*millv1.HarnessSettings{run.DefaultHarness: {Model: model}}
		insertAfter(definition, "clean-tree", review)
	})
}

// gatesOnNode is every user-assigned child of a named node.
func (f *fixture) gatesOnNode(ctx context.Context, ref string) []*daggerv1.Node {
	GinkgoHelper()
	res, err := f.graph.ListNodes(ctx, connect.NewRequest(&daggerv1.ListNodesRequest{
		Project:      project,
		Parent:       ref,
		AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER,
		View:         daggerv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetNodes()
}

// blockedBy is the id of every node with a blocks edge into the run's node.
func (f *fixture) blockedBy(ctx context.Context) []string {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: daggerv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	var blockers []string
	for _, edge := range res.Msg.GetEdges() {
		if edge.GetKind() == daggerv1.EdgeKind_EDGE_KIND_BLOCKS && edge.GetTo() == res.Msg.GetNode().GetId() {
			blockers = append(blockers, edge.GetFrom())
		}
	}
	return blockers
}

// evidenceOf is the evidence that closed a step's latest attempt.
func evidenceOf(detail *millv1.InspectResponse, name string) *millv1.Evidence {
	var latest *millv1.Evidence
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetStepName() == name && d.GetEvidence() != nil {
			latest = d.GetEvidence()
		}
	}
	return latest
}

// answerer is the person at the terminal, scripted: it says the same thing
// to every gate and keeps what it was asked.
type answerer struct {
	approve bool

	mu   sync.Mutex
	asks []run.Ask
}

// Answer records the ask and gives the scripted answer.
func (a *answerer) Answer(_ context.Context, ask run.Ask) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, ask)
	return a.approve, nil
}

// answered is every gate this person was shown.
func (a *answerer) answered() []run.Ask {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]run.Ask{}, a.asks...)
}

var _ run.Answerer = (*answerer)(nil)

// withAnswers puts a person at the run's terminal, which is what `mill run`
// does and what `mill work` never does.
func withAnswers(a *answerer) func(*run.Options) {
	return func(o *run.Options) { o.Answers = a }
}

// after is the step whose attempt started next after the first attempt at a
// named step ended: what the ledger says the current step became.
func after(detail *millv1.InspectResponse, name string) string {
	for i, d := range detail.GetSteps() {
		if d.GetStepRun().GetStepName() != name {
			continue
		}
		if i+1 < len(detail.GetSteps()) {
			return detail.GetSteps()[i+1].GetStepRun().GetStepName()
		}
	}
	return ""
}

// inspectRun reads one run's every attempt by its id, which is how a spec
// looks at a generation the node no longer points at.
func (f *fixture) inspectRun(ctx context.Context, runID string) *millv1.InspectResponse {
	GinkgoHelper()
	res, err := f.store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{Id: runID}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg
}

// branchesDeleted and closingComments are what a restart did to the forge.
func (f *fixtureForge) branchesDeleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.deleted...)
}

func (f *fixtureForge) closingComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.comments...)
}
