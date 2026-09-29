package run_test

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
)

// Resumable is specified against the store alone: the ledger already knows
// which of a worker's own runs an abort left behind, so nothing here reaches
// skift. [fixture.abortedRun] leaves a run exactly as shutdownStep does
// (internal/run/shutdown.go) — one aborted attempt, the run itself still
// `selecting`, the default status [Store.CreateRun] gives a fresh run — which
// is one of the two statuses docs/ledger.md §Resume's resume branch names and
// Resumable lists.
var _ = Describe("Resumable", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	It("lists this worker's own aborted runs, oldest abort first", func() {
		first := f.abortedRun(ctx, project+"#101", f.worker)
		time.Sleep(50 * time.Millisecond)
		second := f.abortedRun(ctx, project+"#102", f.worker)

		candidates, err := f.runner().Resumable(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(2))
		Expect(candidates[0].RunID).To(Equal(first.runID))
		Expect(candidates[0].NodeRef).To(Equal(project + "#101"))
		Expect(candidates[1].RunID).To(Equal(second.runID))
		Expect(candidates[0].UpdatedAt.Before(candidates[1].UpdatedAt)).To(BeTrue(),
			"the earlier abort sorts first")
	})

	It("lists a run this worker left running with no lease and no aborted step", func() {
		drained := f.drainedRun(ctx, project+"#111", f.worker)

		candidates, err := f.runner().Resumable(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		Expect(candidates[0].RunID).To(Equal(drained.runID))
		Expect(candidates[0].NodeRef).To(Equal(project + "#111"))
	})

	It("excludes a run left running by a crash: not aborted, lease still held", func() {
		const lease = "spec-lease-crash"
		created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			LeaseId: lease, NodeId: "spec-node-501", NodeRef: project + "#501", ContextId: "spec-ctx",
			WorkerRef: f.worker, Branch: "mill/crash",
		}))
		Expect(err).NotTo(HaveOccurred())
		runID := created.Msg.GetRun().GetId()
		_, err = f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: lease, RunId: runID, StepName: "implement",
			Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: f.worker,
		}))
		Expect(err).NotTo(HaveOccurred())

		candidates, err := f.runner().Resumable(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(BeEmpty())
	})

	It("excludes a run aborted under a different worker", func() {
		f.abortedRun(ctx, project+"#201", "somebody-else@host")

		candidates, err := f.runner().Resumable(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(BeEmpty())
	})

	It("excludes a run whose latest attempt closed rather than aborted", func() {
		const lease = "spec-lease-closed"
		created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			LeaseId: lease, NodeId: "spec-node-301", NodeRef: project + "#301", ContextId: "spec-ctx",
			WorkerRef: f.worker, Branch: "mill/closed",
		}))
		Expect(err).NotTo(HaveOccurred())
		runID := created.Msg.GetRun().GetId()
		started, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: lease, RunId: runID, StepName: "implement",
			Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: f.worker,
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.store.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: lease, StepRunId: started.Msg.GetStepRun().GetId(),
			Evidence: &millv1.Evidence{
				Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
				Value: &millv1.Evidence_Commit{Commit: strings.Repeat("a", 40)},
			},
		}))
		Expect(err).NotTo(HaveOccurred())

		candidates, err := f.runner().Resumable(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(BeEmpty())
	})

	It("excludes a run that is waiting on a gate", func() {
		// MarkWaiting only admits a running run (internal/domain/transitions.go's
		// runTransitions), so this one is put there through selection rather
		// than [fixture.abortedRun]'s shape: the exclusion under test is the
		// waiting status alone, not an aborted attempt.
		const lease = "spec-lease-waiting"
		created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			LeaseId: lease, NodeId: "spec-node-401", NodeRef: project + "#401", ContextId: "spec-ctx",
			WorkerRef: f.worker, Branch: "mill/waiting",
		}))
		Expect(err).NotTo(HaveOccurred())
		runID := created.Msg.GetRun().GetId()
		selecting, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: lease, RunId: runID, StepName: "select",
			Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: f.worker,
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.store.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
			LeaseId: lease, StepRunId: selecting.Msg.GetStepRun().GetId(), WorkflowId: f.workflow.GetId(),
			Evidence: &millv1.Evidence{
				Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
				Value: &millv1.Evidence_Commit{Commit: strings.Repeat("a", 40)},
			},
		}))
		Expect(err).NotTo(HaveOccurred())
		gate, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
			LeaseId: lease, RunId: runID, StepName: "review",
			Kind: millv1.StepKind_STEP_KIND_GATE, WorkerRef: f.worker,
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.store.MarkWaiting(ctx, connect.NewRequest(&millv1.MarkWaitingRequest{
			LeaseId: lease, RunId: runID, GateNodeId: "gate#1", StepRunId: gate.Msg.GetStepRun().GetId(),
		}))
		Expect(err).NotTo(HaveOccurred())

		candidates, err := f.runner().Resumable(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(BeEmpty())
	})
})

// A fenced heartbeat whose reclaim fails is the other half of Resumable's
// promise: the run has to actually reach `stopped` before a resume can find
// it there, and the write that gets it there has to tolerate a race it
// loses (internal/run/heartbeat.go's reclaimFailedStep).
var _ = Describe("A fenced heartbeat whose reclaim fails", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	It("ends the run stopped with no lease, not left running behind a dead one", func() {
		f.node(ctx, "a held step", "This one is taken away mid-flight and nobody wins it back.")
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})

		runner := f.runner(func(o *run.Options) {
			o.TTL = 12 * time.Second
			o.Interval = 200 * time.Millisecond
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()

		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0))
		first := f.leaseOf(ctx).GetId()
		Expect(first).NotTo(BeEmpty())

		// Force the expiry, then let another principal take the node in
		// skift alone: the worker's own reclaim loses the race, and its
		// heartbeat has to write stopped rather than leave the run behind
		// the lease skift already moved on from.
		f.expire(ctx, first)
		second := f.reclaim(ctx)
		Expect(second).NotTo(Equal(first))

		Eventually(done, 30*time.Second).Should(Receive(MatchError(run.ErrFenced)))

		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))
		Expect(f.runOf(ctx).GetLeaseId()).To(BeEmpty())
	})

	It("leaves another worker's row untouched when its own store-side claim already landed", func() {
		f.node(ctx, "a held step", "This one is taken away mid-flight, and the winner has already claimed it in the ledger too.")
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})

		runner := f.runner(func(o *run.Options) {
			o.TTL = 12 * time.Second
			o.Interval = 200 * time.Millisecond
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()

		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">", 0))
		first := f.leaseOf(ctx).GetId()
		Expect(first).NotTo(BeEmpty())
		runID := f.runOf(ctx).GetId()

		// Force the expiry, then let another principal take the node in
		// skift *and* the ledger: its SetLease lands before this worker's
		// own reclaimFailedStep tries to write, so that write is fenced and
		// must be tolerated rather than clobbering the winner's row.
		f.expire(ctx, first)
		second := f.reclaim(ctx)
		Expect(second).NotTo(Equal(first))
		_, err := f.store.SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
			LeaseId: second, RunId: runID, WorkerRef: "other@host",
		}))
		Expect(err).NotTo(HaveOccurred())

		Eventually(done, 30*time.Second).Should(Receive(MatchError(run.ErrFenced)))

		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
		Expect(f.runOf(ctx).GetLeaseId()).To(Equal(second))
	})
})

// abortedRunResult is what [fixture.abortedRun] leaves behind, for a spec
// that needs to act on the run further.
type abortedRunResult struct {
	runID     string
	stepRunID string
	lease     string
}

// abortedRun opens a run under workerRef and aborts its one attempt at
// "implement", the shape shutdownStep (internal/run/shutdown.go) leaves a
// graceful stop in: the run itself untouched, still `selecting`, with its
// latest step run `aborted`.
func (f *fixture) abortedRun(ctx context.Context, nodeRef, workerRef string) abortedRunResult {
	GinkgoHelper()
	lease := "spec-lease-" + nodeRef
	created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId: lease, NodeId: "spec-node-" + nodeRef, NodeRef: nodeRef, ContextId: "spec-ctx",
		WorkerRef: workerRef, Branch: "mill/" + nodeRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	runID := created.Msg.GetRun().GetId()
	started, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
		LeaseId: lease, RunId: runID, StepName: "implement",
		Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: workerRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	stepRunID := started.Msg.GetStepRun().GetId()
	_, err = f.store.AbortOpenStepRuns(ctx, connect.NewRequest(&millv1.AbortOpenStepRunsRequest{
		LeaseId: lease, RunId: runID, Reason: "shutdown",
	}))
	Expect(err).NotTo(HaveOccurred())
	return abortedRunResult{runID: runID, stepRunID: stepRunID, lease: lease}
}

// drainedRunResult is what [fixture.drainedRun] leaves behind.
type drainedRunResult struct {
	runID     string
	stepRunID string
}

// drainedRun opens a run under workerRef with one step still open and
// releases its lease, the shape drainStep (internal/run/shutdown.go) leaves
// a graceful drain in: the step run left open, not aborted, and the run's
// own lease_id cleared.
func (f *fixture) drainedRun(ctx context.Context, nodeRef, workerRef string) drainedRunResult {
	GinkgoHelper()
	lease := "spec-lease-" + nodeRef
	created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId: lease, NodeId: "spec-node-" + nodeRef, NodeRef: nodeRef, ContextId: "spec-ctx",
		WorkerRef: workerRef, Branch: "mill/" + nodeRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	runID := created.Msg.GetRun().GetId()
	// ReleaseLease only takes a running run, and CreateRun leaves a fresh run
	// `selecting`; SetLease under the same lease is the store's own way to
	// move it to `running`, same as a real claim would.
	_, err = f.store.SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
		LeaseId: lease, RunId: runID, WorkerRef: workerRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	started, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
		LeaseId: lease, RunId: runID, StepName: "implement",
		Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: workerRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	stepRunID := started.Msg.GetStepRun().GetId()
	_, err = f.store.ReleaseLease(ctx, connect.NewRequest(&millv1.ReleaseLeaseRequest{
		LeaseId: lease, RunId: runID,
	}))
	Expect(err).NotTo(HaveOccurred())
	return drainedRunResult{runID: runID, stepRunID: stepRunID}
}

// parkedRun opens a run under a worker ref of its own — always a different
// one than the fixture's own worker, since a parked run is by definition
// nobody's active claim — closes one step, and trips the circuit breaker on
// it: the shape breaker.go's park leaves a run in — status `parked`,
// lease_id cleared, a gate node set — waiting for whatever answers its gate
// to hand it back to the claim loop, not to this worker.
func (f *fixture) parkedRun(ctx context.Context, nodeRef string) {
	GinkgoHelper()
	const workerRef = "somebody-else@host"
	lease := "spec-lease-" + nodeRef
	created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
		LeaseId: lease, NodeId: "spec-node-" + nodeRef, NodeRef: nodeRef, ContextId: "spec-ctx",
		WorkerRef: workerRef, Branch: "mill/" + nodeRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	runID := created.Msg.GetRun().GetId()
	// SetLease is the store's own way to move a fresh run from `selecting` to
	// `running`, same as drainedRun's — Park only admits a running run.
	_, err = f.store.SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
		LeaseId: lease, RunId: runID, WorkerRef: workerRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	started, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
		LeaseId: lease, RunId: runID, StepName: "implement",
		Kind: millv1.StepKind_STEP_KIND_SKILL, WorkerRef: workerRef,
	}))
	Expect(err).NotTo(HaveOccurred())
	_, err = f.store.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
		LeaseId: lease, StepRunId: started.Msg.GetStepRun().GetId(),
		Evidence: &millv1.Evidence{
			Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
			Value: &millv1.Evidence_Commit{Commit: strings.Repeat("a", 40)},
		},
	}))
	Expect(err).NotTo(HaveOccurred())
	_, err = f.store.Park(ctx, connect.NewRequest(&millv1.ParkRequest{
		LeaseId: lease, RunId: runID, GateNodeId: "spec-gate-" + nodeRef,
		Reason: millv1.ParkedReason_PARKED_REASON_RETRIES,
	}))
	Expect(err).NotTo(HaveOccurred())
}
