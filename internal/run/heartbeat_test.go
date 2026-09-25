package run

// The heartbeat's clean/kill distinction, specified directly: stopping a run
// gracefully must never look like a kill to the goroutine renewing its
// lease. Draining is walk()'s own decision at a step boundary and never
// touches the heartbeat at all (internal/run/walk.go's stepAttempted check),
// so there is nothing here to specify about it beyond what these cases
// already show: nothing but stop() or a cancelled context ever reaches
// stepCtx.Done().
//
// The ticker.C branch that renews the lease and turns a fenced or refused
// renewal into h.fail(err) is unchanged by this package's clean/kill split
// and is exercised end to end against a real store and skift by run_test.go's
// "Being fenced out" specs; it is not repeated here.

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("The heartbeat", Label("unit"), func() {
	// An interval long enough that the ticker never fires during a spec:
	// what is under test is stepCtx.Done() alone, never a renewal.
	r := &Runner{options: Options{Interval: time.Hour}}

	It("leaves h.err() nil while running, and nil still after a clean stop", func() {
		h := r.startHeartbeat(context.Background(), nil)
		Expect(h.err()).NotTo(HaveOccurred(), "undisturbed, before stop() is ever called")

		h.stop()
		Expect(h.err()).NotTo(HaveOccurred(), "clean is set before stepCtx is cancelled")
	})

	It("resolves to errShutdown when its parent context is cancelled directly, without stop()", func() {
		ctx, cancel := context.WithCancel(context.Background())
		h := r.startHeartbeat(ctx, nil)

		cancel()

		Eventually(h.err).Should(MatchError(errShutdown),
			"a kill — a second signal or the grace window elapsing — cancels the parent "+
				"context this way, with no stop() to mark it clean first")
	})
})
