package cli_test

import (
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/server"
	"github.com/steading-ai/mill/internal/testsupport"
)

var _ = Describe("mill drain", Label(testsupport.DBLabel), func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("calls the worker's Drain over the RPC and exits zero", func() {
		m := serving(ctx)
		var calls atomic.Int32
		w := workerSocket(ctx, m, server.WorkerServiceConfig{
			Drain: func() { calls.Add(1) },
		})

		out := w.run(ctx, "drain")

		Expect(calls.Load()).To(Equal(int32(1)))
		Expect(out).To(ContainSubstring("drain requested"))
	})

	It("--wait blocks until the worker reports idle", func() {
		m := serving(ctx)
		var probes atomic.Int32
		w := workerSocket(ctx, m, server.WorkerServiceConfig{
			Drain: func() {},
			State: func() millv1.WorkerState {
				// Two draining answers before idle, so the wait actually polls
				// more than once rather than happening to succeed on the first
				// Probe call.
				if probes.Add(1) <= 2 {
					return millv1.WorkerState_WORKER_STATE_DRAINING
				}
				return millv1.WorkerState_WORKER_STATE_IDLE
			},
		})

		out := w.run(ctx, "drain", "--wait", "--timeout", "10s")

		Expect(out).To(ContainSubstring("worker is idle; drain complete"))
		Expect(probes.Load()).To(BeNumerically(">=", 3))
	})

	It("--timeout elapsing exits non-zero, naming the timeout", func() {
		m := serving(ctx)
		w := workerSocket(ctx, m, server.WorkerServiceConfig{
			Drain: func() {},
			State: func() millv1.WorkerState { return millv1.WorkerState_WORKER_STATE_DRAINING },
		})

		out, err := w.try(ctx, "drain", "--wait", "--timeout", "1500ms")

		Expect(err).To(MatchError(ContainSubstring("did not reach idle within 1.5s")), out)
	})
})
