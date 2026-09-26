package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/workspace/machine"
)

// The supervisor is the only place a signal becomes one of the two stops
// docs/architecture.md §"Failure and stop" names, so it is specified here
// with real signals rather than through the worker, which knows nothing
// about them.

var _ = Describe("Supervising a worker", Label("unit"), func() {
	// Long enough that nothing in these specs reaches it by accident; the
	// spec that is about the window sets its own.
	const patient = time.Hour

	// The signals a real process would have received. They are handed to
	// the supervisor rather than sent to this process, because the spec
	// runner has handlers of its own for SIGTERM and this spec is about
	// what the supervisor does with a signal, not about signal.Notify.
	var signals chan os.Signal
	sigterm := func() {
		GinkgoHelper()
		Eventually(signals, time.Minute).Should(BeSent(syscall.SIGTERM))
	}

	BeforeEach(func() { signals = make(chan os.Signal, 2) })

	It("the first signal asks the worker to stop, and a clean stop exits zero", func() {
		worker := newFakeWorker()
		done := make(chan error, 1)
		go func() { done <- supervise(context.Background(), worker, patient, signals) }()

		Eventually(worker.running, time.Minute).Should(BeClosed())
		sigterm()

		Eventually(worker.asked, time.Minute).Should(BeClosed())
		Expect(worker.killed()).To(BeFalse(), "asking is not killing")
		worker.finish(nil)
		Eventually(done, time.Minute).Should(Receive(BeNil()))
	})

	It("a second signal inside the grace window kills the runs in flight", func() {
		worker := newFakeWorker()
		done := make(chan error, 1)
		go func() { done <- supervise(context.Background(), worker, patient, signals) }()

		Eventually(worker.running, time.Minute).Should(BeClosed())
		sigterm()
		Eventually(worker.asked, time.Minute).Should(BeClosed())

		// The worker has not gone: a session is still unwinding. The second
		// signal is what stops waiting for it.
		Consistently(done, 250*time.Millisecond).ShouldNot(Receive())
		sigterm()
		Eventually(done, time.Minute).Should(Receive(MatchError(run.ErrKilled)))
	})

	It("the grace window elapsing kills them too, and being killed exits non-zero", func() {
		worker := newFakeWorker()
		done := make(chan error, 1)
		go func() { done <- supervise(context.Background(), worker, 100*time.Millisecond, signals) }()

		Eventually(worker.running, time.Minute).Should(BeClosed())
		sigterm()
		Eventually(done, time.Minute).Should(Receive(MatchError(run.ErrKilled)))
		Expect(ExitCode(run.ErrKilled)).NotTo(BeZero())
	})

	It("a cancelled context asks the worker to stop, so serve --work stops with the service", func() {
		worker := newFakeWorker()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- supervise(ctx, worker, patient, signals) }()

		Eventually(worker.running, time.Minute).Should(BeClosed())
		cancel()
		Eventually(worker.asked, time.Minute).Should(BeClosed())
		worker.finish(nil)
		Eventually(done, time.Minute).Should(Receive(BeNil()))
	})

	It("one signal reaching both the context and the channel still honours the grace window", func() {
		// A caller whose ctx is itself signal-derived delivers one SIGTERM to
		// both destinations: ctx is cancelled and the same signal lands on
		// the channel, back to back. Seeding both before supervise even
		// starts is what guards this against reverting: whichever of the
		// two the first select inside it happens to pick, the duplicate
		// must not read as a second, unrelated stop.
		worker := newFakeWorker()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		signals <- syscall.SIGTERM
		done := make(chan error, 1)
		go func() { done <- supervise(ctx, worker, patient, signals) }()

		Eventually(worker.running, time.Minute).Should(BeClosed())
		Eventually(worker.asked, time.Minute).Should(BeClosed())
		Consistently(done, 250*time.Millisecond).ShouldNot(Receive(),
			"the duplicate must not read as an unrelated second signal")
		worker.finish(nil)
		Eventually(done, time.Minute).Should(Receive(BeNil()))
	})
})

var _ = Describe("The worker's configuration file", Label("unit"), func() {
	var path string

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(`
project:
  - mill
  - skift
label:
  - agent
concurrency: 4
ttl: 1h
heartbeat: 10m
workflow: feature
skift-socket: /run/skift/other.sock
env_mode: strict
notify_command: /usr/local/bin/thicket-send.sh
workspace: machine
sandbox-root: /srv/mill
gateway: true
gateway-listen: 10.99.255.1:8081
gateway-upstream: https://api.example.test
model-credential-file: /run/credentials/mill.service/claude-oauth
model_credential_minted: 2026-09-01
`), 0o600)).To(Succeed())
	})

	parse := func(args ...string) workOptions {
		GinkgoHelper()
		opts := workOptions{}
		cmd := &cobra.Command{Use: "work"}
		bindWorkFlags(cmd, &opts)
		Expect(cmd.Flags().Parse(append([]string{"--config", path}, args...))).To(Succeed())
		Expect(applyConfig(cmd, &opts)).To(Succeed())
		return opts
	}

	It("fills in every flag the command line left alone", func() {
		opts := parse()
		Expect(opts.projects).To(Equal([]string{"mill", "skift"}))
		Expect(opts.labels).To(Equal([]string{"agent"}))
		Expect(opts.concurrency).To(Equal(4))
		// The lease is the deployment's to size: a host that sleeps longer
		// than the TTL loses every run it held.
		Expect(opts.run.ttl).To(Equal(time.Hour))
		Expect(opts.run.interval).To(Equal(10 * time.Minute))
		Expect(opts.run.workflow).To(Equal("feature"))
		Expect(opts.run.skiftSocket).To(Equal("/run/skift/other.sock"))
		// The environment mode is what a deployment sets and the one
		// setting a unit must not forget; the file says it the way the
		// deployment does.
		Expect(opts.run.envMode).To(Equal("strict"))
		// The notify command is the deployment's, spelled the way env_mode
		// is: a filed gate reaches a person through it.
		Expect(opts.run.notifyCommand).To(Equal("/usr/local/bin/thicket-send.sh"))
		// The workspace kind is the deployment's too: moving a worker from
		// local to machine is this line (docs/sandbox.md §Self-hosting).
		Expect(opts.run.workspace).To(Equal("machine"))
		Expect(opts.run.sandboxRoot).To(Equal("/srv/mill"))
		// The credential gateway is the deployment's as well: on, where it
		// listens, what it forwards to, and the file its credential is in
		// (docs/sandbox.md §Credentials).
		Expect(opts.run.gateway.enabled).To(BeTrue())
		Expect(opts.run.gateway.listen).To(Equal("10.99.255.1:8081"))
		Expect(opts.run.gateway.upstream).To(Equal("https://api.example.test"))
		Expect(opts.run.gateway.credentialFile).To(Equal("/run/credentials/mill.service/claude-oauth"))
	})

	It("refuses a lease TTL it cannot read as a duration", func() {
		Expect(os.WriteFile(path, []byte("ttl: an hour\n"), 0o600)).To(Succeed())
		opts := workOptions{}
		cmd := &cobra.Command{Use: "work"}
		bindWorkFlags(cmd, &opts)
		Expect(cmd.Flags().Parse([]string{"--config", path})).To(Succeed())
		err := applyConfig(cmd, &opts)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ttl"))
	})

	It("defaults the workspace to local and the gateway to off when the file says nothing", func() {
		Expect(os.WriteFile(path, []byte("concurrency: 1\n"), 0o600)).To(Succeed())
		opts := parse()
		Expect(opts.run.workspace).To(Equal("local"))
		// Inside a machine the default is the nested root (docs/sandbox.md
		// §Self-hosting); this suite runs there once mill builds mill.
		expectedRoot := machine.DefaultRoot
		if machine.InsideMachine(context.Background(), cacheDriver()) {
			expectedRoot = machine.NestedRoot
		}
		Expect(opts.run.sandboxRoot).To(Equal(expectedRoot))
		Expect(opts.run.gateway.enabled).To(BeFalse())
		Expect(opts.run.gateway.listen).To(Equal("10.99.255.1:8080"))
		Expect(opts.run.gateway.upstream).To(Equal("https://api.anthropic.com"))
	})

	It("leaves a flag the command line set alone", func() {
		opts := parse("--concurrency", "2", "--project", "just-this-one", "--env-mode", "inherit",
			"--notify-command", "/opt/notify", "--workspace", "local")
		Expect(opts.run.workspace).To(Equal("local"))
		Expect(opts.run.notifyCommand).To(Equal("/opt/notify"))
		Expect(opts.run.envMode).To(Equal("inherit"))
		Expect(opts.concurrency).To(Equal(2))
		Expect(opts.projects).To(Equal([]string{"just-this-one"}))
		Expect(opts.labels).To(Equal([]string{"agent"}), "the rest still comes from the file")
	})

	It("wires the file's retention and retention-projects into the run options", func() {
		Expect(os.WriteFile(path, []byte("retention: twin_days=7\nretention-projects:\n  demo: keep\n"), 0o600)).To(Succeed())
		opts := parse()
		Expect(opts.run.retention).To(Equal("twin_days=7"))
		Expect(opts.run.retentionProjects).To(Equal(map[string]string{"demo": "keep"}))
	})

	It("fails mill work startup on a malformed retention value, naming the key", func() {
		Expect(os.WriteFile(path, []byte("retention: soon\n"), 0o600)).To(Succeed())
		opts := parse()
		cmd := &cobra.Command{Use: "work"}
		cmd.SetOut(GinkgoWriter)
		cmd.SetErr(GinkgoWriter)
		_, err := newRunner(cmd, opts.run, "spec-project", "", nil, nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("retention"))
	})

	It("fails mill work startup on a malformed retention-projects value, naming the project", func() {
		Expect(os.WriteFile(path, []byte("retention-projects:\n  demo: soon\n"), 0o600)).To(Succeed())
		opts := parse()
		cmd := &cobra.Command{Use: "work"}
		cmd.SetOut(GinkgoWriter)
		cmd.SetErr(GinkgoWriter)
		_, err := newRunner(cmd, opts.run, "spec-project", "", nil, nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("retention-projects"))
		Expect(err.Error()).To(ContainSubstring("demo"))
	})

	It("The worker honours the configured socket", func() {
		// Given a config file naming a socket path and no --socket flag
		Expect(os.WriteFile(path, []byte("socket: /tmp/x.sock\n"), 0o600)).To(Succeed())
		// When mill work starts: the command itself, its flags parsed, its
		// file applied, which is the resolution runWork does before it
		// dials anything.
		opts := workOptions{}
		cmd := workCommand(&opts)
		Expect(cmd.ParseFlags([]string{"--config", path})).To(Succeed())
		Expect(applyConfig(cmd, &opts)).To(Succeed())
		// Then it listens on the configured path
		Expect(opts.run.socket).To(Equal("/tmp/x.sock"))
	})

	It("lets a --socket flag win over the file's socket", func() {
		Expect(os.WriteFile(path, []byte("socket: /tmp/x.sock\n"), 0o600)).To(Succeed())
		opts := workOptions{}
		cmd := workCommand(&opts)
		Expect(cmd.ParseFlags([]string{"--config", path, "--socket", "/tmp/flag.sock"})).To(Succeed())
		Expect(applyConfig(cmd, &opts)).To(Succeed())
		Expect(opts.run.socket).To(Equal("/tmp/flag.sock"))
	})

	It("is not required to exist", func() {
		config, err := LoadConfig(filepath.Join(GinkgoT().TempDir(), "absent.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(config).To(Equal(Config{}))
	})

	It("refuses to start on a file it cannot read", func() {
		bad := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(bad, []byte("project: [unclosed\n"), 0o600)).To(Succeed())
		_, err := LoadConfig(bad)
		Expect(err).To(HaveOccurred())
	})
})

// fakeWorker is a worker as its supervisor sees it: it runs until it is
// finished or its context is cancelled, and it records being asked to stop
// without acting on it, which is the case the second signal is for.
type fakeWorker struct {
	running chan struct{}
	asked   chan struct{}
	done    chan error

	mu     sync.Mutex
	once   sync.Once
	wasCut bool
}

func newFakeWorker() *fakeWorker {
	return &fakeWorker{running: make(chan struct{}), asked: make(chan struct{}), done: make(chan error, 1)}
}

func (w *fakeWorker) Run(ctx context.Context) error {
	close(w.running)
	select {
	case err := <-w.done:
		return err
	case <-ctx.Done():
		w.mu.Lock()
		w.wasCut = true
		w.mu.Unlock()
		return run.ErrKilled
	}
}

func (w *fakeWorker) Drain() { w.once.Do(func() { close(w.asked) }) }

// finish is the worker's runs reaching a boundary of their own accord.
func (w *fakeWorker) finish(err error) { w.done <- err }

// killed reports whether the supervisor cancelled the worker's context.
func (w *fakeWorker) killed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wasCut
}
