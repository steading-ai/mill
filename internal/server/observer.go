package server

import (
	"context"
	"sync"
	"time"
)

// DefaultObserverFallback bounds how long a followed Logs stream can lag
// behind a signal this process failed to deliver.
const DefaultObserverFallback = 5 * time.Second

// Observer is what a followed Logs stream waits on instead of polling: one
// broadcast condition per run, signalled by the store's own handlers on
// every write that changes what a reader waits on — StartStepRun,
// CloseStepRun, FailStepRun, MarkWaiting, Park, Done, Fail, CreateRun — and
// by the activity writer on every append (docs/activity.md §Observer). A
// fallback timer bounds every wait, so a write this process forgets to
// signal through cannot hang a stream past it.
//
// The key a caller signals and waits on is a run id for a stream over one
// run; the empty string is the key a stream over every run waits on, so
// CreateRun — which changes no existing run — still wakes it.
type Observer struct {
	fallback time.Duration

	mu    sync.Mutex
	waits map[string]chan struct{}
}

// NewObserver returns an Observer whose Wait never blocks past fallback,
// even if nothing ever signals the key it is waiting on.
func NewObserver(fallback time.Duration) *Observer {
	return &Observer{fallback: fallback, waits: map[string]chan struct{}{}}
}

// Signal wakes every goroutine subscribed to key. A key nobody is watching
// costs a map lookup and nothing else.
func (o *Observer) Signal(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ch, ok := o.waits[key]; ok {
		close(ch)
		delete(o.waits, key)
	}
}

// Subscribe returns the channel that closes the next time Signal(key) is
// called. Call it before checking whatever condition a signal announces
// changed — a read of the file, a reload of the run — so a signal landing
// between the check and Wait is not missed: the channel already exists and
// closes regardless of when the check ran.
func (o *Observer) Subscribe(key string) <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	ch, ok := o.waits[key]
	if !ok {
		ch = make(chan struct{})
		o.waits[key] = ch
	}
	return ch
}

// Wait blocks until ch closes, the fallback timer elapses, or ctx is done,
// whichever comes first.
func (o *Observer) Wait(ctx context.Context, ch <-chan struct{}) {
	timer := time.NewTimer(o.fallback)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
}
