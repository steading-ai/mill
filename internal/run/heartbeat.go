package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
)

// heartbeat holds the lease for the whole run, in a goroutine independent of
// any step.
//
// docs/architecture.md §Heartbeat: skift's TTL is fifteen minutes and a
// single implement step can run an hour, so the worker — not the session —
// renews, for as long as any step runs. A renewal refused is the signal to
// stop: the step's context is cancelled, the run is abandoned locally, and
// nothing further is written on either side.
//
// The same tick reads `cancel_requested` off the run, which is the only
// polling the design allows and the only place a cancellation is noticed.
//
// Stopping is not the heartbeat's concern: a graceful stop is decided by
// walk() at the next step boundary, not by cancelling a step in flight. Only
// a kill — a second signal, or the grace window elapsing, which cancels ctx,
// the step context's parent — reaches the heartbeat, through stepCtx.Done()
// below.
type heartbeat struct {
	// ctx is the context every step runs under. Cancelling it is how a
	// fenced or cancelled run stops what is in flight.
	ctx context.Context

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	// clean is set by stop() before it cancels stepCtx, so the
	// stepCtx.Done() case below can tell a clean stop from an unclean
	// cancellation (a kill) and leave h.reason nil for the former.
	clean atomic.Bool

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
				// A clean stop() already recorded nothing to fail: h.err()
				// stays nil, as it does today. An unclean cancellation — the
				// outer kill context, cancelled by a second signal or the
				// grace window elapsing — resolves to errShutdown itself,
				// since nothing upstream has set a reason for it.
				if !h.clean.Load() {
					h.fail(errShutdown)
				}
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
	if _, err := r.options.Skift.Heartbeat(ctx, s.leaseID(), r.options.TTL); err != nil {
		if !isFenced(err) {
			return err
		}
		// A fenced renewal does not necessarily mean this run lost the
		// race for the node: try to win it back before giving up.
		if reclaimErr := r.reclaim(ctx, s); reclaimErr != nil {
			return errors.Join(errReclaimFailed, ErrFenced, reclaimErr)
		}
	}
	run, _, err := r.runByNode(ctx, s.node.GetId())
	if err != nil {
		// A store that cannot be read is not a reason to drop a lease that
		// skift just renewed. The next tick asks again.
		r.options.Log.Warn("reading the run at a heartbeat", "error", err)
		return nil
	}
	if run.GetCancelRequested() {
		return errCancelled
	}
	return nil
}

// errReclaimFailed marks a fenced heartbeat whose reclaim did not win the
// node back, so walk.go's stopped() can tell it apart from a plain fence and
// write the run `stopped` instead of leaving it `running` with a dead lease.
// It is joined together with ErrFenced, not used to replace it: every other
// caller that checks for a fence still needs to match.
var errReclaimFailed = errors.New("run: reclaim failed")

// reclaim tries to win the run's own node back after its lease expired, so a
// step already in flight can keep running under a fresh lease instead of
// being fenced. It succeeds only when nobody else holds the node: a claim
// refused because another holder has it is left to the caller to fence on,
// exactly as an unreclaimed fenced heartbeat does today.
func (r *Runner) reclaim(ctx context.Context, s *state) error {
	claimed, err := s.skift.Claim(ctx, s.node.GetReference())
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

// reclaimFailedStep writes what an unreclaimed fence means: the attempt this
// worker had open is aborted and the run's lease is given back, both under
// the old lease s.leaseID() still holds, so the next claim — by another
// worker, or this one once it notices the node is open again — resumes it
// rather than finding it stuck `running` behind a lease skift has already
// moved on from.
//
// Both writes present the stale lease as their fencing token, so a race
// where another worker's SetLease already won is tolerated rather than
// treated as a failure: the store's own compare-and-swap refuses the write
// with a fenced error, which is exactly the outcome wanted — that worker's
// row is left untouched (Invariant 9).
func (r *Runner) reclaimFailedStep(ctx context.Context, s *state) error {
	tolerateFenced := func(err error) error {
		if err != nil && !isFenced(err) {
			return err
		}
		return nil
	}
	_, err := call(ctx, r.options.Store.AbortOpenStepRuns, &millv1.AbortOpenStepRunsRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
		Reason:  "fenced",
	})
	if err := tolerateFenced(err); err != nil {
		return err
	}
	_, err = call(ctx, r.options.Store.ReleaseLease, &millv1.ReleaseLeaseRequest{
		LeaseId: s.leaseID(),
		RunId:   s.run.GetId(),
	})
	return tolerateFenced(err)
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

// stop ends the heartbeat and waits for its goroutine. clean is set first, so
// the stepCtx.Done() case this unblocks does not mistake it for a kill.
func (h *heartbeat) stop() {
	h.once.Do(func() {
		h.clean.Store(true)
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

// wait blocks until the heartbeat's own goroutine has recorded why it
// stopped and exited. stepCtx.Done() wakes that goroutine and walk()'s own
// ctx.Err() check concurrently; without waiting here, stopped() can read
// err() before the goroutine's stepCtx.Done() case has called fail(), and
// mistake an unclean stop (a kill) for none at all.
func (h *heartbeat) wait() {
	<-h.done
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
