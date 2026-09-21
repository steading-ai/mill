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
	"github.com/ivy/mill/internal/forge"
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
	// BackoffBase is a node's wait after the first claim the worker could
	// not open, doubled on every consecutive one and capped at BackoffCap.
	// This is the safety net for refusals that are not deterministic enough
	// to Hold (docs/sandbox.md §Reconciliation): it does not need to match
	// the lease TTL, only to stop a bad node from being reclaimed as fast as
	// dagger's round-robin will hand it back (mill#39).
	BackoffBase = time.Second
	// BackoffCap bounds BackoffBase's doubling.
	BackoffCap = 5 * time.Minute
	// backoffSlice is the longest one pass of the claim loop sleeps while a
	// node is within its backoff window. A node's own window can run to
	// BackoffCap, but the loop never sleeps past this slice in one go, so a
	// different, unrelated node's readiness is never starved behind one
	// node's backoff (mill#39).
	backoffSlice = time.Second
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
	// Runner drives every claim. Its dagger client's own project, if any, is
	// only the fallback [Runner.repository] and a bare reference resolve
	// against; the claim itself carries Projects, and every write the claim
	// leads to goes through the client [Runner.drive] scopes to the project
	// the claim actually landed in.
	Runner *Runner
	// Projects is the filter every claim carries. Empty claims from every
	// project the worker's principal may read.
	Projects []string
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
	// SweepInterval is how long the worker goes between sweeps of the
	// ledger against dagger (docs/ledger.md §Sweep). Zero takes
	// [DefaultSweepInterval].
	SweepInterval time.Duration
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
	// lastSweep is when the ledger was last swept against dagger. Touched
	// on the claim loop alone.
	lastSweep time.Time
	// inFlight is every node this process is driving, by id. The sweep
	// leaves those alone whatever dagger says of them: the run's own
	// goroutine is the one that writes its ending.
	inFlight sync.Map
	// killed records that a run was still in flight when the kill context
	// was cancelled, which is the only thing that makes a stop non-zero.
	// The grace window elapsing after the last run has already stopped is
	// an ordinary shutdown.
	killed atomic.Bool
	// loggedNoRepo is every project already warned about having no
	// repository. A worker claiming across many projects holds each node
	// that fails on this the same as any other, but the same broken project
	// saying so on every claim is noise a person has already been told once.
	loggedNoRepo sync.Map
	// backoffMu guards backoff.
	backoffMu sync.Mutex
	// backoff is every node this worker currently owes a wait, by node id:
	// the general safety net for a claim it could not open (mill#39). A
	// node a deterministic refusal already Held is not claimed again
	// regardless, so an entry surviving here past that point is only ever
	// consulted if the Hold write itself failed.
	backoff map[string]*nodeBackoff
}

// nodeBackoff is one node's current refusal streak: until is when the
// worker may claim it for real again, and wait is the delay that reached
// it, doubled on the streak's next refusal and dropped on a real takeover.
type nodeBackoff struct {
	until time.Time
	wait  time.Duration
}

// NewWorker builds a worker, applying the defaults. Every run it drives
// watches its stop channel, so one stop reaches every run in flight.
func NewWorker(options WorkerOptions) (*Worker, error) {
	if options.Runner == nil {
		return nil, errors.New("work: no runner to claim with")
	}
	if options.Concurrency <= 0 {
		options.Concurrency = DefaultConcurrency
	}
	if options.Wait <= 0 {
		options.Wait = DefaultWait
	}
	if options.SweepInterval <= 0 {
		options.SweepInterval = DefaultSweepInterval
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
		backoff:  make(map[string]*nodeBackoff),
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

// start is what precedes the first claim: the sweep of the ledger against
// dagger, then the host's reconciliation, then eviction, in that order and
// all before any lease is taken (docs/ledger.md §Sweep, docs/sandbox.md
// §Reconciliation). The sweep goes first so the host's pass sees the
// ledger's truth: a run the sweep has just failed is no longer live, and
// the machine it could not reach is an orphan and its directory a leftover
// in the very next step.
//
// A ledger that cannot be swept stops the worker the way a host that
// cannot be reconciled does: both are the store or dagger not answering,
// and neither is a state to claim work in.
func (w *Worker) start(ctx context.Context) error {
	if err := w.sweep(ctx); err != nil {
		return fmt.Errorf("work: sweep before claiming: %w", err)
	}
	// A seal that fails is a warning, not a reason to stop: the live file
	// it left behind is safe to retry at the next start.
	if err := w.options.Runner.SealOrphans(ctx); err != nil {
		w.options.Log.Warn("sealing orphaned activity files at start", "error", err)
	}
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

// sweep runs one pass of the sweep and prints its findings, one line each.
// The pass is timed from its start whatever it returned, so a ledger that
// will not answer is asked again at the interval and not at every claim.
func (w *Worker) sweep(ctx context.Context) error {
	w.lastSweep = time.Now()
	report, err := w.options.Runner.Sweep(ctx, w.driving)
	for _, finding := range report.Findings {
		w.printf("sweep: %s\n", finding)
	}
	return err
}

// driving reports whether this process is driving the node right now.
func (w *Worker) driving(nodeID string) bool {
	_, ok := w.inFlight.Load(nodeID)
	return ok
}

// backoffRemaining reports how long is left of a node's current backoff
// window, if it is in one. A window that has already elapsed is treated as
// none: the node is due, whether or not the entry has been swept out yet.
func (w *Worker) backoffRemaining(nodeID string) (time.Duration, bool) {
	w.backoffMu.Lock()
	defer w.backoffMu.Unlock()
	b, ok := w.backoff[nodeID]
	if !ok {
		return 0, false
	}
	remaining := time.Until(b.until)
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

// backoffRefused starts or extends a node's backoff window after a claim
// the worker could not open, doubling the previous wait up to BackoffCap.
// It is only ever called once per actual refusal — [Worker.claim] never
// hands a node to [Worker.drive] while it is still within a window — so the
// caller's one log line per call is already one per refusal, not per cycle.
func (w *Worker) backoffRefused(nodeID string) time.Duration {
	w.backoffMu.Lock()
	defer w.backoffMu.Unlock()
	b, ok := w.backoff[nodeID]
	if !ok {
		b = &nodeBackoff{wait: BackoffBase}
		w.backoff[nodeID] = b
	} else {
		b.wait = min(b.wait*2, BackoffCap)
	}
	b.until = time.Now().Add(b.wait)
	return b.wait
}

// backoffCleared drops a node's backoff streak: a real takeover happened,
// so whatever it owed before is paid.
func (w *Worker) backoffCleared(nodeID string) {
	w.backoffMu.Lock()
	defer w.backoffMu.Unlock()
	delete(w.backoff, nodeID)
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

		// The sweep rides the loop's own cadence rather than a timer of
		// its own: a busy worker passes here between runs, an idle one
		// every Wait, and either sweeps once the interval has elapsed. It
		// runs under the kill context, not the claim's, so a graceful stop
		// does not cut a pass off mid-write. A pass that fails is a warning
		// here and a retry at the next interval, because a run stranded a
		// little longer costs nothing and a worker that stopped claiming
		// over it would.
		if sweepDue(w.lastSweep, w.options.SweepInterval, time.Now()) {
			if err := w.sweep(ctx); err != nil {
				w.options.Log.Warn("sweeping the ledger against dagger", "error", err)
			}
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

		runner := w.options.Runner
		claimed, err := runner.options.Dagger.ClaimNext(claiming, w.options.Projects, w.options.Labels, w.options.Wait)
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

		nodeID := claimed.Node.GetId()

		// A node still within its own backoff window (mill#39): give it
		// straight back, the same as the stop case above, and nothing is
		// written to the run either. The sleep is a bounded slice of the
		// window rather than all of it, so a node still backing off never
		// stalls an unrelated one that is ready right now; the loop simply
		// asks again; a window not yet elapsed is caught here again next
		// time this same node comes back off dagger's round-robin.
		if wait, ok := w.backoffRemaining(nodeID); ok {
			ref := claimed.Node.GetReference()
			if err := runner.options.Dagger.Release(ctx, claimed.Lease.GetId(),
				daggerv1.ReleaseReason_RELEASE_REASON_PREEMPTED,
				fmt.Sprintf("worker: backing off %s, %s left", ref, wait)); err != nil {
				w.options.Log.Warn("releasing a node still backing off", "node", ref, "error", err)
			}
			<-w.slots
			w.sleepBackoff(ctx, min(wait, backoffSlice))
			continue
		}

		w.inFlight.Store(nodeID, struct{}{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-w.slots }()
			defer w.inFlight.Delete(nodeID)
			w.drive(ctx, claimed)
		}()
	}
}

// sleepBackoff waits out one slice of a node's backoff window, cut short by
// a stop or a kill so neither has to wait for it.
func (w *Worker) sleepBackoff(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-w.stopping:
	case <-ctx.Done():
	}
}

// drive runs one claimed node and decides what its ending means to the loop.
//
// Nothing a single run does stops the worker. A failed run has already
// released its lease and recorded why; a fenced one belongs to whoever
// fenced it out and is not this worker's to report. Either way the answer is
// the same: ask for more work.
func (w *Worker) drive(ctx context.Context, claimed *dagger.Claimed) {
	ref := claimed.Node.GetReference()
	nodeID := claimed.Node.GetId()
	err := w.options.Runner.ExecuteClaimed(ctx, claimed, w.stopping)
	if ctx.Err() != nil {
		w.killed.Store(true)
	}
	var missing *forge.NoVCSURLError
	switch {
	case err == nil:
		w.backoffCleared(nodeID)
	case errors.Is(err, ErrFenced):
		w.options.Log.Info("fenced out; the node belongs to another worker", "node", ref)
	case errors.As(err, &missing):
		if _, seen := w.loggedNoRepo.LoadOrStore(missing.Project, struct{}{}); !seen {
			w.options.Log.Warn("a project has no repository; nodes there are held rather than claimed",
				"project", missing.Project, "error", err)
		}
		// Backstop for a Hold write that itself failed (docs/sandbox.md
		// §Reconciliation, mill#39): the node ought to be out of every
		// ready set already, so this window is not expected to be
		// consulted, only to matter if that write was lost.
		w.backoffRefused(nodeID)
	case errors.Is(err, ErrCouldNotOpen):
		wait := w.backoffRefused(nodeID)
		w.options.Log.Warn("could not open a claimed node; backing off before reclaiming it",
			"node", ref, "wait", wait, "error", err)
	default:
		w.options.Log.Error("the run ended badly", "node", ref, "error", err)
		w.printf("%s: %v\n", ref, err)
	}
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
