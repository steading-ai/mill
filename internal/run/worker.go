package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/internal/dagger"
)

// The daemon is the loop, not a scheduler.
//
// docs/architecture.md §"One binary, two faces": there is no scheduler and
// no event consumer. Readiness is dagger's, derived, so there is no "node
// became ready" event to subscribe to; a worker asking for work *is* the
// scheduling. The whole of `mill work` is therefore `ClaimNext --wait`, hand
// what comes back to the same [Runner] `mill run` uses, repeat.
//
// Concurrency is a cap and nothing more. One is O0's value: a semaphore
// taken before the claim, so a lease is never held on work there is no slot
// to run. Above one the code works and nothing here places a run — no
// worktree pools, no affinity — because dagger already orders the ready set
// and a second worker on another host is the design's answer to wanting more
// throughput.

const (
	// DefaultWait is how long one ClaimNext blocks for work. Dagger polls
	// its ready set at most a second apart inside that wait, so a node that
	// becomes ready is claimed about as fast as it became ready, and an
	// idle worker makes one request a minute rather than a busy loop.
	DefaultWait = time.Minute
	// DefaultConcurrency is how many runs a worker drives at once.
	DefaultConcurrency = 1
	// DefaultStopGrace is how long a stopping worker gives its runs to
	// reach a boundary before a second stop kills them.
	DefaultStopGrace = 30 * time.Second
)

// ErrKilled ends the worker when it was stopped a second time inside the
// grace window: the sessions in flight were killed rather than let stop, so
// the process exits non-zero even though the operator asked for it.
var ErrKilled = errors.New("work: killed; sessions were stopped mid-step")

// Startup is what a worker does to its host before it claims anything, in
// this order: reconcile, then evict (docs/sandbox.md §Reconciliation,
// §Eviction). The machine backend implements it — machines against the
// ledger's live runs, leftovers, the scratch sweeps, then the caches and
// images eviction ranks — and the local backend has nothing to do at start.
//
// A reconciliation that cannot run stops the worker before its first claim:
// a host whose machines cannot be told from its live runs is not one to
// start runs on. An eviction that fails is logged and the worker goes on,
// because a cache that was not evicted is a cache that is still there.
type Startup interface {
	// Reconcile is the pass over the host, recorded in the ledger.
	Reconcile(ctx context.Context) error
	// Evict runs eviction, after reconciliation.
	Evict(ctx context.Context) error
}

// WorkerOptions is everything a worker is built from.
type WorkerOptions struct {
	// Runners is one runner per project the worker claims from, polled in
	// turn. A runner carries its own dagger client, and a dagger client is
	// scoped to one project, so "which projects" and "which runners" are
	// the same list.
	Runners []*Runner
	// Startup is run once, before the first claim. Nil does nothing at
	// start, which is the local backend.
	Startup Startup
	// Labels is the label filter every claim carries. A node must hold all
	// of them. Empty claims any node of the project.
	Labels []string
	// Concurrency caps the runs in flight. Zero takes
	// [DefaultConcurrency].
	Concurrency int
	// Wait is how long one claim blocks for work. Zero takes
	// [DefaultWait]; dagger clamps it to five minutes.
	Wait time.Duration
	// Out is where the worker's own progress is written.
	Out io.Writer
	// Log is where diagnostics go. Nil discards them.
	Log *slog.Logger
}

// Worker is `mill work`: the claim loop.
type Worker struct {
	options  WorkerOptions
	slots    chan struct{}
	stopping chan struct{}
	once     sync.Once
	next     int
	// killed records that a run was still in flight when the kill context
	// was cancelled, which is the only thing that makes a stop non-zero.
	// The grace window elapsing after the last run has already stopped is
	// an ordinary shutdown.
	killed atomic.Bool
}

// NewWorker builds a worker, applying the defaults. Every run it drives
// watches its stop channel, so one stop reaches every run in flight.
func NewWorker(options WorkerOptions) (*Worker, error) {
	if len(options.Runners) == 0 {
		return nil, errors.New("work: no project to claim from")
	}
	if options.Concurrency <= 0 {
		options.Concurrency = DefaultConcurrency
	}
	if options.Wait <= 0 {
		options.Wait = DefaultWait
	}
	if options.Log == nil {
		options.Log = slog.New(slog.DiscardHandler)
	}
	if options.Out == nil {
		options.Out = io.Discard
	}
	return &Worker{
		options:  options,
		slots:    make(chan struct{}, options.Concurrency),
		stopping: make(chan struct{}),
	}, nil
}

// Stop asks the worker to stop claiming and every run in flight to reach a
// boundary and leave itself resumable. It returns at once; [Worker.Run] is
// what reports that the last run is done. Stopping twice is not an error.
func (w *Worker) Stop() {
	w.once.Do(func() { close(w.stopping) })
}

// Run claims and drives until the worker is stopped.
//
// ctx is the kill: cancelling it does not ask, it ends the sessions in
// flight and returns [ErrKilled]. The graceful stop is [Worker.Stop].
func (w *Worker) Run(ctx context.Context) error {
	if err := w.start(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	err := w.claim(ctx, &wg)
	wg.Wait()
	if err == nil && w.killed.Load() {
		return ErrKilled
	}
	return err
}

// start is what precedes the first claim: reconciliation, then eviction,
// in that order and both before any lease is taken (docs/sandbox.md
// §Reconciliation).
func (w *Worker) start(ctx context.Context) error {
	if w.options.Startup == nil {
		return nil
	}
	if err := w.options.Startup.Reconcile(ctx); err != nil {
		return fmt.Errorf("work: reconcile before claiming: %w", err)
	}
	if err := w.options.Startup.Evict(ctx); err != nil {
		w.options.Log.Warn("eviction at start", "error", err)
	}
	return nil
}

// claim is the loop itself: a slot, a claim, a run, repeat.
func (w *Worker) claim(ctx context.Context, wg *sync.WaitGroup) error {
	// A claim is a blocking request that may have most of a minute left to
	// run, and a worker asked to stop must not spend it waiting for work it
	// would refuse. The claim therefore runs under a context of its own,
	// which a stop ends; the runs already in flight are untouched by it.
	claiming, endClaims := context.WithCancel(ctx)
	defer endClaims()
	go func() {
		select {
		case <-w.stopping:
			endClaims()
		case <-claiming.Done():
		}
	}()

	for {
		// Checked before the select below rather than only in it: once
		// stopping is closed both of its cases are ready, and a select
		// would take either.
		if w.stopped() || ctx.Err() != nil {
			return nil
		}

		// The slot comes before the claim on purpose. A lease taken with
		// nowhere to run it is a node held out of everybody else's ready
		// set for no reason, and the worker would be heartbeating nothing.
		select {
		case w.slots <- struct{}{}:
		case <-w.stopping:
			return nil
		case <-ctx.Done():
			return nil
		}

		runner := w.turn()
		claimed, err := runner.options.Dagger.ClaimNext(claiming, w.options.Labels, w.options.Wait)
		if err != nil || claimed == nil {
			<-w.slots
			if err == nil || w.stopped() || ctx.Err() != nil {
				continue
			}
			// Dagger being unreachable is not something a worker can work
			// around; the unit restarts it. Reporting rather than
			// swallowing is what makes that restart happen.
			return fmt.Errorf("work: claim: %w", err)
		}

		// A claim that landed as the stop did. Give it straight back rather
		// than open a run only to stop it: nothing has been written yet, so
		// the node simply returns to the ready set.
		if w.stopped() {
			if err := runner.options.Dagger.Release(ctx, claimed.Lease.GetId(),
				daggerv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "shutdown"); err != nil {
				w.options.Log.Warn("releasing a claim that landed as the worker stopped",
					"node", claimed.Node.GetReference(), "error", err)
			}
			<-w.slots
			return nil
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-w.slots }()
			w.drive(ctx, runner, claimed)
		}()
	}
}

// drive runs one claimed node and decides what its ending means to the loop.
//
// Nothing a single run does stops the worker. A failed run has already
// released its lease and recorded why; a fenced one belongs to whoever
// fenced it out and is not this worker's to report. Either way the answer is
// the same: ask for more work.
func (w *Worker) drive(ctx context.Context, runner *Runner, claimed *dagger.Claimed) {
	ref := claimed.Node.GetReference()
	err := runner.ExecuteClaimed(ctx, claimed, w.stopping)
	if ctx.Err() != nil {
		w.killed.Store(true)
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrFenced):
		w.options.Log.Info("fenced out; the node belongs to another worker", "node", ref)
	default:
		w.options.Log.Error("the run ended badly", "node", ref, "error", err)
		w.printf("%s: %v\n", ref, err)
	}
}

// turn is the next runner to claim from, round-robin over the projects. It
// is called from the claim loop alone, which is single-threaded.
func (w *Worker) turn() *Runner {
	runner := w.options.Runners[w.next%len(w.options.Runners)]
	w.next++
	return runner
}

// stopped reports whether a graceful stop has been asked for.
func (w *Worker) stopped() bool {
	select {
	case <-w.stopping:
		return true
	default:
		return false
	}
}

// printf writes one line of the worker's progress.
func (w *Worker) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(w.options.Out, format, args...)
}
