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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/gen/skift/v1/skiftv1connect"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
	"github.com/steading-ai/mill/internal/domain"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/server"
	"github.com/steading-ai/mill/internal/skift"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workflow"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/local"
	"github.com/steading-ai/mill/internal/workspace/redact"
)

// The run loop is specified end to end, because that is what it is: the only
// thing it does that no other package does is put skift, the ledger, the
// workspace, a harness and a forge in one order. A fixture that faked any of
// the first three would specify the fake.
//
// Real: skift, the ledger over PostgreSQL, git, the workspace. Scripted:
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
	skift *testsupport.Skift
	// db is the ledger's own database, for the one read the store's API
	// does not offer.
	db        *pgxpool.Pool
	client    *skift.Client
	graph     skiftv1connect.GraphServiceClient
	store     millv1connect.StoreServiceClient
	workflow  *millv1.Workflow
	harness   *fake.Harness
	forge     *fixtureForge
	origin    string
	root      string
	worker    string
	nodeRef   string
	nodeTitle string

	// redactor is the one the workspace and the run's step logs share, as
	// they do in `mill run`: a value the session registers with the `redact`
	// verb is scrubbed from the log written after it.
	redactor *redact.Redactor
	// workspace is the one the last call to Workspaces made, so a spec can
	// act on it the way a session does through the socket.
	workspace run.Workspace

	// keepWorkspace leaves the run's checkout on disk after the run
	// finishes, for a spec that reads the branch the run left behind.
	keepWorkspace bool
	// wrapWorkspace, when set, wraps the workspace the run is given, for a
	// spec that watches what the loop asks of it.
	wrapWorkspace func(run.Workspace) run.Workspace

	// theirs counts the commits a spec made under somebody else's
	// identity, so each one is a commit and not an empty write.
	theirs atomic.Int64

	// mu guards made.
	mu sync.Mutex
	// made is every commit a scripted session left on the run's branch, in
	// order. The `commit` step closes on one of them, so a spec that says
	// which one is saying what the step's evidence has to be.
	made []string
}

// commits is what a scripted session does to the checkout: it writes a file
// and commits it on the run's branch through the workspace's own git verb, so
// the commit carries the mill's identity and signature exactly as a session's
// `git.commit` does.
//
// A `commit` step closes only on a commit that was not on the branch when the
// attempt started (docs/domain-model.md §Evidence), so a scripted session
// that leaves the tree alone leaves that step nothing to close on. Which is
// the point: the specs that make a commit and the specs that do not are
// asserting opposite things.
func (f *fixture) commits(harness.Session) {
	defer GinkgoRecover()
	ctx := context.Background()
	f.mu.Lock()
	n := len(f.made) + 1
	f.mu.Unlock()

	name := fmt.Sprintf("session-%d.txt", n)
	Expect(f.workspace).NotTo(BeNil(), "the run made a workspace before the session ran")
	Expect(f.workspace.Write(ctx, name, []byte("what the session changed\n"))).To(Succeed())
	info, err := f.workspace.Git().Commit(ctx, workspace.CommitRequest{
		Message: fmt.Sprintf("the session's change %d", n),
	})
	Expect(err).NotTo(HaveOccurred())

	f.mu.Lock()
	f.made = append(f.made, info.SHA)
	f.mu.Unlock()
}

// commitsMade is every commit the scripted sessions made, in order.
func (f *fixture) commitsMade() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.made...)
}

// head is where the run's branch points now, read the way the run loop reads
// it: through the workspace, not through the session.
func (f *fixture) head(ctx context.Context) string {
	GinkgoHelper()
	commits, err := f.workspace.Git().Log(ctx, workspace.LogRequest{Limit: 1})
	Expect(err).NotTo(HaveOccurred())
	Expect(commits).NotTo(BeEmpty())
	return commits[0].SHA
}

// start builds the world: a skift with a project in it, the ledger behind
// its own Connect handlers, the quick-fix workflow pushed, and a repository
// to clone.
func start(ctx context.Context) *fixture {
	GinkgoHelper()
	service := pg.StartSkift(ctx)
	httpClient := service.HTTPClient()

	f := &fixture{
		skift:    service,
		client:   skift.New(httpClient, service.BaseURL(), project, skift.WithAgentRef("spec@host"), skift.WithSessionRef("spec")),
		graph:    skiftv1connect.NewGraphServiceClient(httpClient, service.BaseURL()),
		forge:    &fixtureForge{},
		worker:   "spec@host",
		redactor: redact.New(),
	}

	_, err := f.graph.CreateProject(ctx, connect.NewRequest(&skiftv1.CreateProjectRequest{
		RequestId: "mill-spec-project", Title: "mill specs", Slug: project,
	}))
	Expect(err).NotTo(HaveOccurred())

	f.db = pg.Database(ctx)
	srv := server.New(server.Config{Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil))},
		server.NewStoreService(domain.New(f.db), server.NewObserver(server.DefaultObserverFallback), GinkgoT().TempDir()), nil)
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
	f.workflow = f.pushWorkflowFrom(ctx, workflowDir)
}

// pushWorkflowFrom stores one workflow directory: every skill it pins, then
// the definition. A spec that runs a workflow other than quick-fix pushes it
// on top; the store holds both, as a worker's does.
func (f *fixture) pushWorkflowFrom(ctx context.Context, dir string) *millv1.Workflow {
	GinkgoHelper()
	definition, skills, err := workflow.Load(dir)
	Expect(err).NotTo(HaveOccurred())
	for _, skill := range skills {
		_, err := f.store.PutSkill(ctx, connect.NewRequest(&millv1.PutSkillRequest{Skill: skill}))
		Expect(err).NotTo(HaveOccurred())
	}
	stored, err := f.store.PutWorkflow(ctx, connect.NewRequest(&millv1.PutWorkflowRequest{Workflow: definition}))
	Expect(err).NotTo(HaveOccurred())
	return stored.Msg.GetWorkflow()
}

// node files the work the run is for and remembers its reference.
func (f *fixture) node(ctx context.Context, title, body string) string {
	GinkgoHelper()
	res, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-node-" + title,
		Project:   project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CreateNode{CreateNode: &skiftv1.CreateNodeOp{
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
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-done-" + title,
		Project:   project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
			Node: ref, Result: &skiftv1.Result{Body: result},
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
	return ref
}

// blocks makes from a blocking predecessor of to.
func (f *fixture) blocks(ctx context.Context, from, to string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-edge-" + from + to,
		Project:   project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_AddEdge{AddEdge: &skiftv1.AddEdgeOp{
			From: from, To: to, Kind: skiftv1.EdgeKind_EDGE_KIND_BLOCKS,
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
		Store:      f.store,
		Skift:      f.client,
		Workspaces: f.workspaces(),
		Attach:     f.attached(),
		Repositories: func(context.Context, *skiftv1.Project) (run.Repository, error) {
			return run.Repository{Forge: f.forge, Origin: f.origin}, nil
		},
		Harnesses:   func(run.Workspace) (*harness.Registry, error) { return registry, nil },
		WorkerRef:   f.worker,
		Workflow:    "quick-fix",
		WorkflowDir: mustAbs(workflowDir),
		Out:         GinkgoWriter,
		Log:         slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		Redactor:    f.redactor,
	}
	for _, option := range options {
		option(&opts)
	}
	return run.New(opts)
}

// workspaces makes a local workspace under the spec's temporary directory,
// reattaching one an earlier invocation left behind.
func (f *fixture) workspaces() run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec, repo run.Repository) (run.Workspace, error) {
		local := local.New(local.Options{
			Root:     filepath.Join(f.root, "workspaces"),
			Origin:   repo.Origin,
			Forge:    repo.Forge,
			Redactor: f.redactor,
		})
		var ws run.Workspace = local
		if f.keepWorkspace {
			ws = keeping{Workspace: ws}
		}
		if f.wrapWorkspace != nil {
			ws = f.wrapWorkspace(ws)
		}
		f.workspace = ws
		if _, err := local.Attach(ctx, spec); err == nil {
			return ws, nil
		}
		if _, err := local.Create(ctx, spec); err != nil {
			return nil, err
		}
		return ws, nil
	}
}

// attached finds the workspace an earlier invocation left behind and never
// makes one: the seam a restart and the sweep destroy through.
func (f *fixture) attached() run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec, repo run.Repository) (run.Workspace, error) {
		local := local.New(local.Options{
			Root:     filepath.Join(f.root, "workspaces"),
			Origin:   repo.Origin,
			Forge:    repo.Forge,
			Redactor: f.redactor,
		})
		if _, err := local.Attach(ctx, spec); err != nil {
			return nil, err
		}
		return local, nil
	}
}

// drop closes a node as a person does — no lease, no mill request id —
// which skift admits only once no live lease holds it. It is how a spec
// strands a run: the node is gone from every ready set, and nothing will
// claim it to end the run underneath.
func (f *fixture) drop(ctx context.Context, ref string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-drop-" + ref, Project: project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_DropNode{DropNode: &skiftv1.DropNodeOp{
			Node: ref, Reason: skiftv1.DropReason_DROP_REASON_SUPERSEDED, Detail: "the spec filed it again",
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// selectingRun opens a run on the node holding no lease, as a restart that
// created the next generation leaves it, under the worker named.
func (f *fixture) selectingRun(ctx context.Context, workerRef string) *millv1.Run {
	GinkgoHelper()
	node, err := f.client.Node(ctx, f.nodeRef)
	Expect(err).NotTo(HaveOccurred())
	res, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		NodeId: node.GetId(), NodeRef: node.GetReference(), ContextId: node.GetContextId(),
		Generation: 1, WorkerRef: workerRef, Branch: workspace.BranchFor(node.GetReference()),
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetRun()
}

// keeping is the run's workspace with Destroy taken out. A run that finishes
// removes its checkout, which is what it should do and what leaves a spec
// asking git about the branch nothing to ask.
type keeping struct{ run.Workspace }

func (keeping) Destroy(context.Context) error { return nil }

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

// renderedLog is one attempt's activity record, decoded and rendered back
// into the lines `mill logs` would print — what a spec asserting on log
// content reads, rather than the frames themselves.
func renderedLog(dir, runID, step string, attempt int64) string {
	GinkgoHelper()
	out, err := run.RenderedLog(dir, runID, step, attempt)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// script fixes what the harness replays, one entry per turn. The last is
// reused, so a spec that cares about one turn names one.
func (f *fixture) script(scripts ...fake.Script) {
	f.harness = fake.New(scripts...)
}

// quickFixScripts is one clean pass of quick-fix: four skill steps, each
// ending well, each billing something. Every turn leaves a commit on the
// branch, because the `commit` step closes on a commit that was not there
// when it started and on nothing else.
//
// Every turn rather than the commit step's alone: scripts are replayed in
// order, the last is reused once they run out, and the fake harness has no
// idea which step it is replaying. A run resumed mid-workflow, or one a
// check sends round again, reaches the commit step on a different script
// every time, and a turn that committed nothing there would fail the step
// for a reason the spec is not about. A step whose evidence is not `commit`
// is unaffected by a head that moved.
func (f *fixture) quickFixScripts() []fake.Script {
	return []fake.Script{
		{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
		{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
		{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
		{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
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
	mu     sync.Mutex
	pr     forge.PullRequest
	merged bool
	merges int
	// refuseQueue is consumed one error per Merge call, in order; once it
	// runs dry, Merge falls through to its ordinary success.
	refuseQueue []error
	checksAt    forge.Status
	// checksQueue is consumed one status per Checks call, in order; once it
	// runs dry, Checks falls through to checksAt (or forge.Success). A spec
	// that needs to fail the checks on one head and not another — the head a
	// conflict's own catch-up produces, say — uses this rather than checksAt,
	// which cannot tell one call from the next.
	checksQueue []forge.Status
	checks      int
	comments    []string
	deleted     []string
}

// mergeCalls is how many times the forge was asked to merge the pull
// request, so a spec can tell a merge that never reached the forge (a
// catch-up conflict) from one that did.
func (f *fixtureForge) mergeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merges
}

// checksCalls is how many times the forge was asked about a head's checks,
// so a spec can prove a head that was merged was checked exactly once, and
// never again once it had been (mill#62).
func (f *fixtureForge) checksCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

// Authenticate is the credential the workspace hands a clone or a push. The
// fixture's origin is a path on this host, so there is nothing to prove and
// nothing to inject.
func (f *fixtureForge) Authenticate(context.Context) (forge.GitAuth, error) {
	return forge.GitAuth{}, nil
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if len(f.checksQueue) > 0 {
		status := f.checksQueue[0]
		f.checksQueue = f.checksQueue[1:]
		return status, nil
	}
	if f.checksAt == "" {
		return forge.Success, nil
	}
	return f.checksAt, nil
}

func (f *fixtureForge) Merge(_ context.Context, pr forge.PullRequest) (forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges++
	if len(f.refuseQueue) > 0 {
		err := f.refuseQueue[0]
		f.refuseQueue = f.refuseQueue[1:]
		return pr, err
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

// seedTarget commits one file directly onto the origin's default branch,
// standing in for another run's PR merging into main while this one is still
// in flight: the fact `merge`'s catch-up has to notice and act on.
func (f *fixture) seedTarget(name, content string) {
	GinkgoHelper()
	work := filepath.Join(f.root, "origin-work")
	Expect(os.WriteFile(filepath.Join(work, name), []byte(content), 0o644)).To(Succeed())
	git(work, "add", name)
	git(work, "commit", "-m", "advance main")
	git(work, "push", f.origin, "main")
}

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
	gitAs(dir, nil, args...)
}

// gitAs runs one git command with extra environment: the identity a commit
// is made under, when a spec needs one that is not the mill's.
func gitAs(dir string, env map[string]string, args ...string) {
	GinkgoHelper()
	environment := map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"}
	for name, value := range env {
		environment[name] = value
	}
	process, err := workspace.Spawner{Floor: scratchEnv()}.Exec(context.Background(), workspace.Command{
		Path: "git", Args: args, Dir: dir,
		Env: environment,
	})
	Expect(err).NotTo(HaveOccurred())
	stdout, stderr := readAll(process.Stdout), readAll(process.Stderr)
	result := process.Wait()
	Expect(result.Status).To(BeZero(), "git %s: %s %s", strings.Join(args, " "), stdout(), stderr())
}

// scratchEnv is the environment for the fixture's own git, with the host's
// GIT_* variables cleared: under a git hook the process inherits GIT_DIR and
// GIT_INDEX_FILE, and this git would otherwise act on the hook's repository
// rather than the fixture, whatever Dir says.
func scratchEnv() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			clean = append(clean, kv)
		}
	}
	return clean
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

// result is the node's result once skift says it is done.
func (f *fixture) result(ctx context.Context) (*skiftv1.Result, error) {
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&skiftv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: skiftv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetResult(), nil
}

// leaseOf is the live lease on the node, or nothing.
func (f *fixture) leaseOf(ctx context.Context) *skiftv1.Lease {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&skiftv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: skiftv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetLease()
}

// expire takes the node's lease away, which is what the run loop's next
// heartbeat has to notice. Skift's own expiry is a timer; a release is the
// same end state reached at a time the spec chooses.
func (f *fixture) expire(ctx context.Context, leaseID string) {
	GinkgoHelper()
	Expect(f.client.Release(ctx, leaseID,
		skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "spec: forcing expiry")).To(Succeed())
}

// reclaim is a second client taking the node, and returns its lease id.
func (f *fixture) reclaim(ctx context.Context) string {
	GinkgoHelper()
	other := skift.New(f.skift.HTTPClient(), f.skift.BaseURL(), project,
		skift.WithAgentRef("other@host"), skift.WithSessionRef("other"))
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

// skiftUsage is what skift recorded against the node's attempt, by model.
// Skift sums the records of a lease, so posting the same step twice would
// double what this returns.
func (f *fixture) skiftUsage(ctx context.Context) map[string]int64 {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&skiftv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: skiftv1.View_VIEW_FULL,
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
func artifactNames(result *skiftv1.Result) []string {
	names := make([]string, 0, len(result.GetArtifacts()))
	for _, artifact := range result.GetArtifacts() {
		names = append(names, artifact.GetName())
	}
	return names
}

// referenceOf is the URI of a result's named reference artifact.
func referenceOf(result *skiftv1.Result, name string) string {
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

// patchBreaker sits between the run loop and skift and refuses the next
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
		return nil, errors.New("the patch never reached skift")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p.socket)
		},
	}
	return transport.RoundTrip(req)
}

// breaking builds a skift client whose patches this spec can interrupt.
func (f *fixture) breaking() (*skift.Client, *patchBreaker) {
	breaker := &patchBreaker{socket: f.skift.Socket}
	return skift.New(&http.Client{Transport: breaker}, f.skift.BaseURL(), project), breaker
}

// arming wraps the store client so that the completing patch's request id
// being minted is what arms the breaker: the very next skift write is the
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

// refused is a session every request of which the upstream turned away: no
// request it made was ever answered, so it carries no verdict, only an
// outage (mill#57).
func refused(final string) fake.Script {
	return fake.Script{Terminal: &harness.Terminal{
		FinalMessage: final,
		Exit:         harness.ProcessExit{Status: 1, Reason: harness.ReasonUpstream},
		Usage:        usage(1),
	}}
}

// gatesOn is every user-assigned child of the node: the gates, and nothing
// else, because nothing else in these specs is assigned to a person.
func (f *fixture) gatesOn(ctx context.Context) []*skiftv1.Node {
	GinkgoHelper()
	return f.gatesOnNode(ctx, f.nodeRef)
}

// theGate is the node's one gate, and fails the spec when there is not
// exactly one.
func (f *fixture) theGate(ctx context.Context) *skiftv1.Node {
	GinkgoHelper()
	gates := f.gatesOn(ctx)
	Expect(gates).To(HaveLen(1), "exactly one gate is filed")
	return gates[0]
}

// comments are what the node carries, oldest first.
func (f *fixture) comments(ctx context.Context) []string {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&skiftv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: skiftv1.View_VIEW_FULL,
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
// person who denies drops the gate, and skift never lets the node it blocks
// become ready again. The run that records a denial is therefore the one
// still holding the lease, which is single-run mode; see the specs that put
// an [answerer] at the terminal.
func (f *fixture) approveGate(ctx context.Context, ref string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-answer-" + ref, Project: project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
			Node: ref, Result: &skiftv1.Result{Body: "the spec says yes"},
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// approveGateEmpty completes a gate with a blank result, the way a person
// leaves blank at the terminal. skift itself refuses a truly empty body
// (internal/domain.ErrResultRequired), so this sends whitespace, which
// clears that check but still leaves `gate` evidence with nothing to read
// (internal/evidence.verifyGate trims before comparing) — how a spec fails
// resumeGate's evidence.Verify call without reaching inside skift (mill#30).
func (f *fixture) approveGateEmpty(ctx context.Context, ref string) {
	GinkgoHelper()
	_, err := f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: "spec-answer-empty-" + ref, Project: project,
		Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
			Node: ref, Result: &skiftv1.Result{Body: "   "},
		}}}},
	}))
	Expect(err).NotTo(HaveOccurred())
}

// openPath is whether this process still holds path open under any fd —
// true of an activity file whose writer was never closed.
func openPath(path string) bool {
	entries, err := os.ReadDir("/proc/self/fd")
	Expect(err).NotTo(HaveOccurred())
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && link == path {
			return true
		}
	}
	return false
}

// writerLeaked answers mill#30's two questions about an attempt once its
// Execute call has returned: is its activity file still open, the writer
// never closed; and does a verb the spec runs afterward, through the
// workspace the run left behind, still land in that attempt's record — the
// sink still attached. Both are false when the attempt cleaned up after
// itself, whatever it failed on.
func (f *fixture) writerLeaked(ctx context.Context, logDir, step string, attempt int64) (open, grew bool) {
	GinkgoHelper()
	runID := f.runOf(ctx).GetId()
	path := run.ActivityPath(logDir, runID, step, attempt)
	open = openPath(path)
	before, err := run.ReadActivities(logDir, runID, step, attempt)
	Expect(err).NotTo(HaveOccurred())
	f.commits(harness.Session{})
	after, err := run.ReadActivities(logDir, runID, step, attempt)
	Expect(err).NotTo(HaveOccurred())
	return open, len(after) > len(before)
}

// assertWriterClosed checks both halves of a leaked writer through
// [fixture.writerLeaked] for step's latest attempt: the activity file is no
// longer open, and a verb run afterward is not appended to its record.
func (f *fixture) assertWriterClosed(ctx context.Context, logDir, step string) {
	GinkgoHelper()
	sr := stepRuns(f.inspect(ctx), step)[0]
	open, grew := f.writerLeaked(ctx, logDir, step, sr.GetAttempt())
	Expect(open).To(BeFalse(), "the writer stayed open")
	Expect(grew).To(BeFalse(), "a verb run after the attempt died was appended to its record")
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
// The step is a parameter because the helper is about any step; that every
// spec so far asks about `implement` is a fact about those specs.
//
//nolint:unparam // see above
func sessionsOf(detail *millv1.InspectResponse, name string) []string {
	var ids []string
	for _, sr := range stepRuns(detail, name) {
		ids = append(ids, sr.GetSessionId())
	}
	return ids
}

// failingStore refuses one named write, which is how a spec stages a
// process that died between two writes of an ordered sequence. MarkWaiting
// is always refused, for the gate-filing specs this was written for; refuse
// additionally names one other RPC among the leak points mill#30's specs
// need to fail, or is empty to refuse nothing else.
type failingStore struct {
	millv1connect.StoreServiceClient
	refuse string
	calls  atomic.Int64
}

func (f *failingStore) MarkWaiting(context.Context, *connect.Request[millv1.MarkWaitingRequest]) (*connect.Response[millv1.MarkWaitingResponse], error) {
	f.calls.Add(1)
	return nil, errors.New("the process died before the store write")
}

func (f *failingStore) IssueStepToken(ctx context.Context, req *connect.Request[millv1.IssueStepTokenRequest]) (*connect.Response[millv1.IssueStepTokenResponse], error) {
	if f.refuse == "IssueStepToken" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.IssueStepToken(ctx, req)
}

func (f *failingStore) RecordInstall(ctx context.Context, req *connect.Request[millv1.RecordInstallRequest]) (*connect.Response[millv1.RecordInstallResponse], error) {
	if f.refuse == "RecordInstall" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.RecordInstall(ctx, req)
}

func (f *failingStore) RecordUsage(ctx context.Context, req *connect.Request[millv1.RecordUsageRequest]) (*connect.Response[millv1.RecordUsageResponse], error) {
	if f.refuse == "RecordUsage" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.RecordUsage(ctx, req)
}

func (f *failingStore) RevokeStepToken(ctx context.Context, req *connect.Request[millv1.RevokeStepTokenRequest]) (*connect.Response[millv1.RevokeStepTokenResponse], error) {
	if f.refuse == "RevokeStepToken" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.RevokeStepToken(ctx, req)
}

func (f *failingStore) MarkUsagePosted(ctx context.Context, req *connect.Request[millv1.MarkUsagePostedRequest]) (*connect.Response[millv1.MarkUsagePostedResponse], error) {
	if f.refuse == "MarkUsagePosted" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.MarkUsagePosted(ctx, req)
}

func (f *failingStore) CloseStepRun(ctx context.Context, req *connect.Request[millv1.CloseStepRunRequest]) (*connect.Response[millv1.CloseStepRunResponse], error) {
	if f.refuse == "CloseStepRun" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.CloseStepRun(ctx, req)
}

func (f *failingStore) FailStepRun(ctx context.Context, req *connect.Request[millv1.FailStepRunRequest]) (*connect.Response[millv1.FailStepRunResponse], error) {
	if f.refuse == "FailStepRun" {
		f.calls.Add(1)
		return nil, errors.New("the process died before the store write")
	}
	return f.StoreServiceClient.FailStepRun(ctx, req)
}

// rotateFailing is a workspace whose token rotation always fails, the way a
// step's credential could fail to install into the socket without ever
// touching the step it was meant to authorize (mill#30).
type rotateFailing struct {
	run.Workspace
}

func (rotateFailing) Rotate(context.Context, workspace.Token) error {
	return errors.New("the token never reached the workspace")
}

// installing is a workspace that reports one install, the way
// workspace/machine's sandboxed backend does over the socket's dnf shim and
// workspace/local — what these specs actually run against — does not
// (workspace.Reporter). Without it, recordInstalls has nothing to give the
// store and its own leak point (mill#30) is untestable.
type installing struct {
	run.Workspace
}

func (installing) ReportInstall(context.Context, []string) error { return nil }

func (installing) Installs() []workspace.Install {
	return []workspace.Install{{Packages: []string{"left-pad"}}}
}

var (
	_ workspace.Reporter = installing{}
	_ run.Workspace      = rotateFailing{}
)

// releases records every lease release the run made, with its reason, by
// reading the requests on their way to skift. The reason is part of the
// contract — a gate, a budget stop and a preemption are three different
// things to whoever reads skift's leases afterwards — and nothing skift
// returns afterwards says which one was sent.
type releases struct {
	socket string
	mu     sync.Mutex
	seen   []skiftv1.ReleaseReason
}

func (r *releases) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/Release") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		var release skiftv1.ReleaseRequest
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
func (r *releases) reasons() []skiftv1.ReleaseReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]skiftv1.ReleaseReason{}, r.seen...)
}

// watching builds a skift client whose releases this spec can read.
func (f *fixture) watching() (*skift.Client, *releases) {
	watcher := &releases{socket: f.skift.Socket}
	return skift.New(&http.Client{Transport: watcher}, f.skift.BaseURL(), project,
		skift.WithAgentRef(f.worker), skift.WithSessionRef("spec")), watcher
}

// patchFailure refuses an ApplyPatch that carries an op this spec is
// watching for, and forwards every other patch untouched. It is how a spec
// fails one write inside a call the run loop makes several of — denyGate's
// Complete, or a step's own artifact attach — without reaching inside the
// client (mill#30).
type patchFailure struct {
	socket string
	match  func(*skiftv1.PatchOp) bool
}

func (p *patchFailure) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/ApplyPatch") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		var patch skiftv1.ApplyPatchRequest
		if err := proto.Unmarshal(body, &patch); err == nil {
			for _, op := range patch.GetOps() {
				if p.match(op) {
					return nil, errors.New("the patch never reached skift")
				}
			}
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", p.socket)
		},
	}
	return transport.RoundTrip(req)
}

// failingPatches builds a skift client whose ApplyPatch this spec can fail
// selectively, by op, rather than for every patch the way [fixture.breaking]
// does.
func (f *fixture) failingPatches(match func(*skiftv1.PatchOp) bool) *skift.Client {
	return skift.New(&http.Client{Transport: &patchFailure{socket: f.skift.Socket, match: match}}, f.skift.BaseURL(), project)
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
	// commits is what every other turn does to the checkout. A review reads
	// and never writes; every other session in these specs is one that could
	// have committed, and the `commit` step closes on nothing else.
	commits func(harness.Session)
	// until is how many review turns fail before one succeeds. Zero is a
	// reviewer that never finds the work good, which is what the circuit
	// breaker's specs need.
	until int

	mu       sync.Mutex
	rounds   int
	sessions []string
	verbs    []string
	// prompts is what each review turn was given, in order, so a spec can
	// read what the step was told between its own attempts.
	prompts []string
}

// Start begins a turn.
func (r *reviewer) Start(_ context.Context, session harness.Session, prompt harness.Prompt, _ []harness.Attachment) (*harness.Turn, error) {
	return r.play("start", session, prompt), nil
}

// Resume continues one.
func (r *reviewer) Resume(_ context.Context, session harness.Session, prompt harness.Prompt, _ []harness.Attachment) (*harness.Turn, error) {
	return r.play("resume", session, prompt), nil
}

// Cancel ends a turn that is not running, which is not an error.
func (r *reviewer) Cancel(context.Context, harness.Session) error { return nil }

func (r *reviewer) play(verb string, session harness.Session, prompt harness.Prompt) *harness.Turn {
	r.mu.Lock()
	r.sessions = append(r.sessions, session.ID)
	r.verbs = append(r.verbs, verb)
	reviewing := session.Options["model"] == r.model
	if reviewing {
		r.rounds++
		r.prompts = append(r.prompts, prompt.Text)
	}
	round := r.rounds
	failing := reviewing && (r.until == 0 || round <= r.until)
	r.mu.Unlock()

	turn := harness.NewTurn()
	go func() {
		if failing {
			turn.Finish(&harness.Terminal{
				FinalMessage: fmt.Sprintf("round %d: the change still does the wrong thing", round),
				Exit:         harness.ProcessExit{Status: 1},
				Usage:        usage(1),
			}, nil)
			return
		}
		if !reviewing && r.commits != nil {
			r.commits(session)
		}
		turn.Finish(fake.Terminal("done", nil, usage(1)), nil)
	}()
	return turn
}

// reviewPrompts is the prompt each review turn was given, in order.
func (r *reviewer) reviewPrompts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.prompts...)
}

var _ harness.Harness = (*reviewer)(nil)

// withHarness runs the loop against an adapter of the spec's own, in place
// of the scripted one.
func withHarness(adapter harness.Harness) func(*run.Options) {
	return func(o *run.Options) {
		registry := harness.NewRegistry()
		Expect(registry.Register(run.DefaultHarness, adapter)).To(Succeed())
		o.Harnesses = func(run.Workspace) (*harness.Registry, error) { return registry, nil }
	}
}

// options is an options document as a step carries it.
func options(document map[string]any) *structpb.Struct {
	GinkgoHelper()
	out, err := structpb.NewStruct(document)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// reviewedFix is quick-fix with a review step after the check, which is what
// the revise and circuit-breaker specs need: a step that fails, sends the
// work back to implement, and eventually runs out of attempts.
func (f *fixture) reviewedFix(ctx context.Context, model string) string {
	GinkgoHelper()
	return f.variant(ctx, "reviewed-fix", func(definition *millv1.Workflow) {
		review := reviewStep(stepOf(definition, "implement").GetSkill())
		review.Harness = map[string]*millv1.HarnessSettings{
			run.DefaultHarness: {Options: options(map[string]any{"model": model})},
		}
		insertAfter(definition, "clean-tree", review)
	})
}

// gatesOnNode is every user-assigned child of a named node.
func (f *fixture) gatesOnNode(ctx context.Context, ref string) []*skiftv1.Node {
	GinkgoHelper()
	res, err := f.graph.ListNodes(ctx, connect.NewRequest(&skiftv1.ListNodesRequest{
		Filter: &skiftv1.NodeFilter{
			Projects:     []string{project},
			Parent:       ref,
			AssigneeKind: skiftv1.AssigneeKind_ASSIGNEE_KIND_USER,
		},
		View: skiftv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	return res.Msg.GetNodes()
}

// blockedBy is the id of every node with a blocks edge into the run's node.
func (f *fixture) blockedBy(ctx context.Context) []string {
	GinkgoHelper()
	res, err := f.graph.GetNode(ctx, connect.NewRequest(&skiftv1.GetNodeRequest{
		Project: project, Node: f.nodeRef, View: skiftv1.View_VIEW_FULL,
	}))
	Expect(err).NotTo(HaveOccurred())
	var blockers []string
	for _, edge := range res.Msg.GetEdges() {
		if edge.GetKind() == skiftv1.EdgeKind_EDGE_KIND_BLOCKS && edge.GetTo() == res.Msg.GetNode().GetId() {
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

// openStepRun is the attempt in flight, which is what a cancellation stops.
func (f *fixture) openStepRun(ctx context.Context) *millv1.StepRun {
	GinkgoHelper()
	for _, step := range f.inspect(ctx).GetSteps() {
		if step.GetStepRun().GetStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING {
			return step.GetStepRun()
		}
	}
	return nil
}

// stepRunStatus is where one attempt ended up.
func (f *fixture) stepRunStatus(ctx context.Context, stepRunID string) millv1.StepRunStatus {
	GinkgoHelper()
	for _, step := range f.inspect(ctx).GetSteps() {
		if step.GetStepRun().GetId() == stepRunID {
			return step.GetStepRun().GetStatus()
		}
	}
	return millv1.StepRunStatus_STEP_RUN_STATUS_UNSPECIFIED
}

// abortReason is what the audit trail says an attempt was aborted for. The
// reason is on the event rather than the row: the row holds where the
// attempt ended, the trail holds why.
func (f *fixture) abortReason(ctx context.Context, stepRunID string) string {
	GinkgoHelper()
	res, err := f.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{NodeRef: f.nodeRef}))
	Expect(err).NotTo(HaveOccurred())
	for _, event := range res.Msg.GetEvents() {
		if event.GetOp() == "abort_step_run" && event.GetStepRunId() == stepRunID {
			return event.GetAfter().GetFields()["reason"].GetStringValue()
		}
	}
	return ""
}

// eventOps is the op name of every event the ledger recorded for the node's
// run, oldest first, so a spec can assert what was and was not written
// without knowing the store's own schema.
func (f *fixture) eventOps(ctx context.Context) []string {
	GinkgoHelper()
	res, err := f.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{NodeRef: f.nodeRef}))
	Expect(err).NotTo(HaveOccurred())
	ops := make([]string, 0, len(res.Msg.GetEvents()))
	for _, event := range res.Msg.GetEvents() {
		ops = append(ops, event.GetOp())
	}
	return ops
}

// releasedLease is why the last lease on the node ended. Skift returns the
// live lease on a node and nothing about the one before it, so its own
// record is read directly: the release reason is on the lease row and the
// detail is on the event, which skift writes as "reason: detail".
func (f *fixture) releasedLease(ctx context.Context) (reason, detail string) {
	GinkgoHelper()
	conn, err := pgx.Connect(ctx, f.skift.DatabaseURL)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close(ctx) }()

	node, err := f.client.Node(ctx, f.nodeRef)
	Expect(err).NotTo(HaveOccurred())

	var recorded string
	Expect(conn.QueryRow(ctx, `
		SELECT coalesce(reason, '') FROM events
		WHERE node_id = $1 AND op = 'release_lease'
		ORDER BY occurred_at DESC, id DESC LIMIT 1`, node.GetId()).Scan(&recorded)).To(Succeed())
	reason, detail, _ = strings.Cut(recorded, ": ")
	return reason, detail
}
