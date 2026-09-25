package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// heartbeat holds the lease for the whole run, in a goroutine independent of
// any step.
//
// docs/architecture.md §Heartbeat: dagger's TTL is fifteen minutes and a
// single implement step can run an hour, so the worker — not the session —
// renews, for as long as any step runs. A renewal refused is the signal to
// stop: the step's context is cancelled, the run is abandoned locally, and
// nothing further is written on either side.
//
// The same tick reads `cancel_requested` off the run, which is the only
// polling the design allows and the only place a cancellation is noticed.
//
// The goroutine also watches the run's stop channel, so a worker shutting
// down reaches the step through the one seam that already stops it rather
// than through a second path of its own.
type heartbeat struct {
	// ctx is the context every step runs under. Cancelling it is how a
	// fenced or cancelled run stops what is in flight.
	ctx context.Context

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu     sync.Mutex
	reason error
}

// startHeartbeat renews the lease until the returned heartbeat is stopped.
func (r *Runner) startHeartbeat(ctx context.Context, s *state) *heartbeat {
	stepCtx, cancel := context.WithCancel(ctx)
	h := &heartbeat{ctx: stepCtx, cancel: cancel, done: make(chan struct{})}

	go func() {
		defer close(h.done)
		ticker := time.NewTicker(r.options.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-stepCtx.Done():
				return
			case <-s.stopping:
				// The process is going away. It is the same shape as a
				// cancellation — stop the step, abort the attempt, give the
				// lease back — and differs only in the detail dagger
				// records and in nobody having asked for this run in
				// particular (docs/architecture.md §"Failure and stop").
				h.fail(errShutdown)
				return
			case <-ticker.C:
				if err := r.beat(stepCtx, s); err != nil {
					h.fail(err)
					return
				}
			}
		}
	}()
	return h
}

// beat is one tick: renew the lease, then read the cancel flag.
func (r *Runner) beat(ctx context.Context, s *state) error {
	if _, err := r.options.Dagger.Heartbeat(ctx, s.leaseID(), r.options.TTL); err != nil {
		if !isFenced(err) {
			return err
		}
		// A fenced renewal does not necessarily mean this run lost the
		// race for the node: try to win it back before giving up.
		if reclaimErr := r.reclaim(ctx, s); reclaimErr != nil {
			return errors.Join(ErrFenced, reclaimErr)
		}
	}
	run, _, err := r.runByNode(ctx, s.node.GetId())
	if err != nil {
		// A store that cannot be read is not a reason to drop a lease that
		// dagger just renewed. The next tick asks again.
		r.options.Log.Warn("reading the run at a heartbeat", "error", err)
		return nil
	}
	if run.GetCancelRequested() {
		return errCancelled
	}
	return nil
}

// reclaim tries to win the run's own node back after its lease expired, so a
// step already in flight can keep running under a fresh lease instead of
// being fenced. It succeeds only when nobody else holds the node: a claim
// refused because another holder has it is left to the caller to fence on,
// exactly as an unreclaimed fenced heartbeat does today.
func (r *Runner) reclaim(ctx context.Context, s *state) error {
	claimed, err := s.dagger.Claim(ctx, s.node.GetReference())
	if err != nil {
		r.options.Log.Warn("reclaiming a fenced node: claim refused",
			"node", s.node.GetReference(), "run", s.run.GetId(), "error", err)
		return err
	}
	if _, err := call(ctx, r.options.Store.SetLease, &millv1.SetLeaseRequest{
		LeaseId:   claimed.Lease.GetId(),
		RunId:     s.run.GetId(),
		WorkerRef: r.options.WorkerRef,
	}); err != nil {
		r.options.Log.Warn("reclaiming a fenced node: setting the new lease failed",
			"node", s.node.GetReference(), "run", s.run.GetId(), "error", err)
		return err
	}
	s.setLease(claimed.Lease.GetId())
	r.options.Log.Debug("reclaimed a fenced node",
		"node", s.node.GetReference(), "run", s.run.GetId(), "lease", claimed.Lease.GetId())
	return nil
}

// fail records why the heartbeat stopped and cancels the step.
func (h *heartbeat) fail(err error) {
	h.mu.Lock()
	if h.reason == nil {
		h.reason = err
	}
	h.mu.Unlock()
	h.cancel()
}

// stop ends the heartbeat and waits for its goroutine.
func (h *heartbeat) stop() {
	h.once.Do(func() {
		h.cancel()
		<-h.done
	})
}

// err is why the heartbeat stopped, or nil when it was simply stopped.
func (h *heartbeat) err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

// containsAny reports whether text holds any of the marks, case-insensitively.
func containsAny(text string, marks ...string) bool {
	lower := strings.ToLower(text)
	for _, mark := range marks {
		if strings.Contains(lower, mark) {
			return true
		}
	}
	return false
}
