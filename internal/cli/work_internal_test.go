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

	"github.com/ivy/mill/internal/run"
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
})

var _ = Describe("The worker's configuration file", Label("unit"), func() {
	var path string

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(`
project:
  - mill
  - dagger
label:
  - agent
concurrency: 4
workflow: feature
dagger-socket: /run/dagger/other.sock
env_mode: strict
`), 0o600)).To(Succeed())
	})

	parse := func(args ...string) workOptions {
		GinkgoHelper()
		opts := workOptions{}
		cmd := &cobra.Command{Use: "work"}
		bindWorkFlags(cmd, &opts)
		Expect(cmd.Flags().Parse(append([]string{"--config", path}, args...))).To(Succeed())
		var socket string
		Expect(applyConfig(cmd, &opts, &socket)).To(Succeed())
		return opts
	}

	It("fills in every flag the command line left alone", func() {
		opts := parse()
		Expect(opts.projects).To(Equal([]string{"mill", "dagger"}))
		Expect(opts.labels).To(Equal([]string{"agent"}))
		Expect(opts.concurrency).To(Equal(4))
		Expect(opts.run.workflow).To(Equal("feature"))
		Expect(opts.run.daggerSocket).To(Equal("/run/dagger/other.sock"))
		// The environment mode is what a deployment sets and the one
		// setting a unit must not forget; the file says it the way the
		// deployment does.
		Expect(opts.run.envMode).To(Equal("strict"))
	})

	It("leaves a flag the command line set alone", func() {
		opts := parse("--concurrency", "2", "--project", "just-this-one", "--env-mode", "inherit")
		Expect(opts.run.envMode).To(Equal("inherit"))
		Expect(opts.concurrency).To(Equal(2))
		Expect(opts.projects).To(Equal([]string{"just-this-one"}))
		Expect(opts.labels).To(Equal([]string{"agent"}), "the rest still comes from the file")
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

func (w *fakeWorker) Stop() { w.once.Do(func() { close(w.asked) }) }

// finish is the worker's runs reaching a boundary of their own accord.
func (w *fakeWorker) finish(err error) { w.done <- err }

// killed reports whether the supervisor cancelled the worker's context.
func (w *fakeWorker) killed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wasCut
}
