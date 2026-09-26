package cli_test

import (
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
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

	It("a drain that outlasts its timeout leaves the run alone", func() {
		m := serving(ctx)
		r := openRunOn(ctx, m, "millcli#80", nodeID(80))
		// Opened and never closed: the run is genuinely mid-step, the lease
		// still its own, not a stub standing in for one.
		r.start(ctx, "gather-context", millv1.StepKind_STEP_KIND_SKILL)

		w := workerSocket(ctx, m, server.WorkerServiceConfig{
			Drain:    func() {},
			State:    func() millv1.WorkerState { return millv1.WorkerState_WORKER_STATE_DRAINING },
			InFlight: func() []run.RunningNode { return []run.RunningNode{{NodeRef: r.nodeRef}} },
		})

		out, err := w.try(ctx, "drain", "--wait", "--timeout", "1500ms")

		Expect(err).To(MatchError(ContainSubstring(r.nodeRef)), out)
		reloaded := r.reload(ctx)
		Expect(reloaded.GetLeaseId()).To(Equal(r.lease),
			"a drain that never reaches a boundary must not touch the run's lease")
		Expect(reloaded.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
	})
})
