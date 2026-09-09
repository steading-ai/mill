package acceptance_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
)

// The mill half of the suite: the binary built from this tree, one `mill
// serve` for the whole suite, and `mill run` driven as a subprocess exactly
// as an operator drives it.
//
// `mill run` is a subprocess rather than a library call because two of the
// three scenarios are about what happens to the process — killed mid-step,
// fenced out of its lease — and neither is observable from inside it.

// serviceReady bounds how long `mill serve` has to bind its socket.
const serviceReady = 30 * time.Second

// service is the suite's `mill serve`: the store every run writes its ledger
// to, and the socket the specs read it back over.
type service struct {
	Socket string
	Store  millv1connect.StoreServiceClient
}

// suite is what the preflight and the BeforeSuite between them set up, held
// for every spec.
var suite struct {
	service *service
}

// The suite is set up once for the whole run, not once per process: one
// build, one `mill serve`, one workflow push, however many processes the
// scenarios are spread over. Each scenario still gets a scratch project, a
// dagger project and a workspace root of its own, so they share the store
// and nothing else.
var _ = SynchronizedBeforeSuite(func(ctx SpecContext) []byte {
	if !enabled() {
		Skip("set " + EnabledVar + "=1 to run the acceptance suite; see docs/runbook.md for what it needs")
	}
	env = preflight()
	env.Mill = buildMill(ctx)
	suite.service = startService()
	pushWorkflow(ctx)
	shared, err := json.Marshal(sharedSetup{Mill: env.Mill, Socket: suite.service.Socket})
	Expect(err).NotTo(HaveOccurred())
	return shared
}, func(shared []byte) {
	if !enabled() {
		Skip("set " + EnabledVar + "=1 to run the acceptance suite; see docs/runbook.md for what it needs")
	}
	env = preflight()
	var setup sharedSetup
	Expect(json.Unmarshal(shared, &setup)).To(Succeed())
	env.Mill = setup.Mill
	suite.service = connectService(setup.Socket)
})

// sharedSetup is what the first process hands the others: the binary it
// built and the socket the service it started listens on.
type sharedSetup struct {
	Mill   string `json:"mill"`
	Socket string `json:"socket"`
}

// buildMill compiles the binary under test from this tree. The suite tests
// the tree it lives in, so it never reaches for a mill that happens to be
// installed.
func buildMill(ctx context.Context) string {
	GinkgoHelper()
	dir, err := os.MkdirTemp("", "mill-acceptance-bin-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() error { return os.RemoveAll(dir) })

	binary := filepath.Join(dir, "mill")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/mill") //nolint:gosec // the suite building its own subject
	build.Dir = env.RepoRoot
	build.Stdout, build.Stderr = GinkgoWriter, GinkgoWriter
	Expect(build.Run()).To(Succeed(), "building ./cmd/mill")
	return binary
}

// startService runs `mill serve` on a socket of the suite's own, against the
// database the environment named, and returns a client for it.
func startService() *service {
	GinkgoHelper()
	dir, err := os.MkdirTemp("", "mill-acceptance-svc-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() error { return os.RemoveAll(dir) })

	socket := filepath.Join(dir, "mill.sock")
	// Not CommandContext: ginkgo cancels a node's context when the node
	// returns, and this service has to outlive the setup that started it. It
	// is killed by the cleanup below, at the end of the whole suite.
	serve := exec.Command(env.Mill, "serve", "--socket", socket, "--database-url", env.DatabaseURL) //nolint:gosec // the suite's own binary
	serve.Stdout, serve.Stderr = GinkgoWriter, GinkgoWriter
	serve.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	Expect(serve.Start()).To(Succeed())
	DeferCleanup(func() {
		_ = syscall.Kill(-serve.Process.Pid, syscall.SIGKILL)
		_ = serve.Wait()
	})

	Eventually(func() error {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return err
		}
		return conn.Close()
	}, serviceReady, 50*time.Millisecond).Should(Succeed(), "mill serve bound its socket")
	return connectService(socket)
}

// connectService is a client for a service already listening, which is what
// every process but the first has.
func connectService(socket string) *service {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	return &service{Socket: socket, Store: millv1connect.NewStoreServiceClient(client, "http://mill")}
}

// pushWorkflow loads this tree's workflow definitions into the store, which
// is what a deployment does at install time. Both of them: a worker holds
// every workflow its projects may select, and `feature` reaches for skills
// `quick-fix` carries.
func pushWorkflow(ctx context.Context) {
	GinkgoHelper()
	for _, name := range []string{"quick-fix", "feature"} {
		push := exec.CommandContext(ctx, env.Mill, "workflow", "push", workflowDir(name), "--socket", suite.service.Socket) //nolint:gosec // the suite's own binary
		push.Stdout, push.Stderr = GinkgoWriter, GinkgoWriter
		Expect(push.Run()).To(Succeed(), "mill workflow push %s", name)
	}
}

// workflowDir is one workflow definition in this tree.
func workflowDir(name string) string { return filepath.Join(env.RepoRoot, "workflows", name) }

// inspect reads a node's run out of the ledger: the run, the workflow it
// follows, and every attempt with its evidence, session and cost.
func inspect(ctx context.Context, nodeRef string) *millv1.InspectResponse {
	GinkgoHelper()
	res, err := suite.service.Store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{NodeRef: nodeRef}))
	Expect(err).NotTo(HaveOccurred(), "inspecting %s", nodeRef)
	return res.Msg
}

// tryInspect is inspect for a caller polling for a run that may not exist
// yet.
func tryInspect(ctx context.Context, nodeRef string) *millv1.InspectResponse {
	res, err := suite.service.Store.Inspect(ctx, connect.NewRequest(&millv1.InspectRequest{NodeRef: nodeRef}))
	if err != nil {
		return nil
	}
	return res.Msg
}

// worker is one `mill run` subprocess: started, watched, killed, waited on.
type worker struct {
	command *exec.Cmd
	output  *lockedBuffer

	done chan struct{}
	mu   sync.Mutex
	err  error
}

// runOptions are the knobs a scenario changes. Everything else about the
// invocation is the same in all three.
type runOptions struct {
	// TTL and Heartbeat shorten the lease the run holds. The fencing
	// scenario wants a heartbeat inside its own patience, not the
	// production five minutes.
	TTL       time.Duration
	Heartbeat time.Duration
	// Workflow and Tier are the run's selection. Empty is quick-fix, which
	// is what every scenario but the feature one wants.
	Workflow string
	Tier     string
	// WorkflowDir is where the selected workflow was loaded from, which a
	// check step's relative command resolves against. Empty is this tree's
	// own `workflows/<name>`.
	WorkflowDir string
	// SkillOverrides run a step on a skill read from a directory instead of
	// the one its workflow pins, step name to directory. It is the seam the
	// seeded bad implementation is written through, and `mill run` refuses
	// it unless MILL_ACCEPTANCE is set (docs/runbook.md).
	SkillOverrides map[string]string
}

// start invokes `mill run` for a node, against the suite's service, the
// host's dagger, and the scenario's scratch project. It returns as soon as
// the process is running.
func start(ctx context.Context, s *staged, opts runOptions) *worker {
	GinkgoHelper()
	selected, tier := opts.Workflow, opts.Tier
	if selected == "" {
		selected, tier = "quick-fix", "quick-fix"
	}
	dir := opts.WorkflowDir
	if dir == "" {
		dir = workflowDir(selected)
	}
	args := []string{
		"run", s.NodeRef,
		"--socket", suite.service.Socket,
		"--dagger-socket", env.DaggerSocket,
		"--project", s.Graph.Slug,
		"--workflow", selected,
		"--tier", tier,
		"--workflow-dir", dir,
		"--vcs-url", s.VCSURL,
		"--origin", s.Origin,
		"--ref", s.Ref,
		"--workspace-dir", s.WorkspaceDir,
		"--log-dir", s.LogDir,
		"--sidecar", env.Sidecar,
		"--no-ask",
	}
	if opts.TTL > 0 {
		args = append(args, "--ttl", opts.TTL.String())
	}
	if opts.Heartbeat > 0 {
		args = append(args, "--heartbeat", opts.Heartbeat.String())
	}
	for step, from := range opts.SkillOverrides {
		args = append(args, "--skill-override", step+"="+from)
	}

	command := exec.CommandContext(ctx, env.Mill, args...) //nolint:gosec // the suite's own binary
	// The forge credential reaches the mill the way a deployment gives it
	// one: through the environment, never through argv.
	command.Env = append(os.Environ(),
		"MILL_FORGE_TOKEN="+env.ForgeToken,
		"MILL_FORGE_USER="+env.ForgeUser)
	// A process group of its own, so killing the run kills the sidecar and
	// everything under it rather than orphaning a session.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	output := &lockedBuffer{}
	command.Stdout = teeTo(output)
	command.Stderr = teeTo(output)
	Expect(command.Start()).To(Succeed(), "mill run %s", s.NodeRef)

	w := &worker{command: command, output: output, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		err := command.Wait()
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
	}()
	DeferCleanup(func() { w.kill() })
	return w
}

// wait blocks until the run ends and returns its exit status.
func (w *worker) wait(timeout time.Duration) int {
	GinkgoHelper()
	Eventually(w.done, timeout, time.Second).Should(BeClosed(), "mill run ended:\n%s", w.output.String())
	w.mu.Lock()
	err := w.err
	w.mu.Unlock()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	Expect(err).NotTo(HaveOccurred(), "waiting on mill run:\n%s", w.output.String())
	return -1
}

// kill ends the whole process group at once, which is what a crash looks
// like from the outside: no shutdown, no cleanup, no lease released.
func (w *worker) kill() {
	if w.command.Process == nil {
		return
	}
	_ = syscall.Kill(-w.command.Process.Pid, syscall.SIGKILL)
	<-w.done
}

// running reports whether the process is still going.
func (w *worker) running() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

// runningStep polls the ledger for an attempt of a named step that is
// running, which is how a scenario finds the moment to pull the plug. It is
// the ledger and not the log, because the ledger is what the design says is
// the truth.
func runningStep(ctx context.Context, nodeRef, step string, timeout time.Duration) *millv1.StepRun {
	GinkgoHelper()
	var found *millv1.StepRun
	Eventually(func() *millv1.StepRun {
		detail := tryInspect(ctx, nodeRef)
		for _, d := range detail.GetSteps() {
			sr := d.GetStepRun()
			if sr.GetStepName() == step && sr.GetStatus() == millv1.StepRunStatus_STEP_RUN_STATUS_RUNNING {
				found = sr
				return sr
			}
		}
		return nil
	}, timeout, 2*time.Second).ShouldNot(BeNil(), "step %s reached running on %s", step, nodeRef)
	return found
}

// attempts are a step's attempts in order.
func attempts(detail *millv1.InspectResponse, step string) []*millv1.StepRunDetail {
	var found []*millv1.StepRunDetail
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetStepName() == step {
			found = append(found, d)
		}
	}
	return found
}

// latest is the last attempt of every step, by step name.
func latest(detail *millv1.InspectResponse) map[string]*millv1.StepRunDetail {
	byStep := map[string]*millv1.StepRunDetail{}
	for _, d := range detail.GetSteps() {
		name := d.GetStepRun().GetStepName()
		if before, ok := byStep[name]; !ok || d.GetStepRun().GetAttempt() > before.GetStepRun().GetAttempt() {
			byStep[name] = d
		}
	}
	return byStep
}

// workspaceDirs are the run directories under a workspace root, which is
// what "a second workspace directory" means. The signing directory is the
// mill account's key and belongs to no run.
func workspaceDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "signing" {
			dirs = append(dirs, entry.Name())
		}
	}
	return dirs
}

// lockedBuffer collects a subprocess's output for a failure message, from
// the two goroutines exec gives it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// teeTo sends a stream to the buffer and to the spec's own report.
func teeTo(buffer *lockedBuffer) *tee { return &tee{buffer: buffer} }

type tee struct{ buffer *lockedBuffer }

func (t *tee) Write(p []byte) (int, error) {
	_, _ = GinkgoWriter.Write(p)
	return t.buffer.Write(p)
}

// watcher collects every workspace directory that appears under a root while
// a run is in flight. A resume that cloned again would create a second
// directory and destroy it when the run finished, leaving nothing to see
// afterwards, so the question has to be asked while the run is going.
type watcher struct {
	done chan struct{}

	mu   sync.Mutex
	seen map[string]bool
}

// watchWorkspaces starts polling, seeded with the directories already there.
func watchWorkspaces(root string, initial []string) *watcher {
	w := &watcher{done: make(chan struct{}), seen: map[string]bool{}}
	for _, dir := range initial {
		w.seen[dir] = true
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				dirs := workspaceDirs(root)
				w.mu.Lock()
				for _, dir := range dirs {
					w.seen[dir] = true
				}
				w.mu.Unlock()
			}
		}
	}()
	return w
}

// stop ends the watch and returns every directory it ever saw.
func (w *watcher) stop() map[string]bool {
	close(w.done)
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := make(map[string]bool, len(w.seen))
	for dir := range w.seen {
		seen[dir] = true
	}
	return seen
}
