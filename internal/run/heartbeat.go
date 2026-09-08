package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
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
	if _, err := r.options.Dagger.Heartbeat(ctx, s.lease, r.options.TTL); err != nil {
		if isFenced(err) {
			return errors.Join(ErrFenced, err)
		}
		return err
	}
	run, err := r.runByNode(ctx, s.node.GetId())
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

// errCancelled is a cancellation a human asked for. The step is aborted, the
// lease released with preempted, and the run stays resumable at that step.
var errCancelled = errors.New("run: cancelled")

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

// cancelStep is what a cancellation does to the run: the attempt is aborted,
// the lease is released with preempted, and the run is left resumable.
func (r *Runner) cancelStep(ctx context.Context, s *state) error {
	if _, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
		LeaseId: s.lease,
		RunId:   s.run.GetId(),
		Reason:  "cancelled",
	}); err != nil {
		return err
	}
	if err := r.options.Dagger.Release(ctx, s.lease, daggerv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "cancel"); err != nil {
		return err
	}
	r.printf("cancelled; run %s stays resumable\n", s.run.GetId())
	return errReleased
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
