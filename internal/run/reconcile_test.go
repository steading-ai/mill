package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
)

// The loop's half of docs/sandbox.md §Reconciliation: what a worker does
// before its first claim, and how a run whose sandbox is gone ends. The
// host's half — machines against live runs, leftovers, the sweeps — is the
// machine backend's, specified in internal/workspace/machine.

// recordingStartup is a Startup that notes the order it was called in,
// beside the claims the counting transport sees.
type recordingStartup struct {
	mu    sync.Mutex
	order []string
	claim func() int64
	fail  error
	// also, when set, is something else the pass observes at each call and
	// records beside it: the status of a run, say.
	also func() string
}

func (s *recordingStartup) Reconcile(context.Context) error {
	s.note("reconcile")
	return s.fail
}

func (s *recordingStartup) Evict(context.Context) error {
	s.note("evict")
	return nil
}

func (s *recordingStartup) note(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := fmt.Sprintf("%s (claims so far: %d)", what, s.claim())
	if s.also != nil {
		entry += " " + s.also()
	}
	s.order = append(s.order, entry)
}

func (s *recordingStartup) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// resuming is a workspace with a step boundary that was attached mid-step:
// its next ordinal is the reaped slice's, and it records every slice begun.
type resuming struct {
	run.Workspace
	next  int
	mu    sync.Mutex
	begun []int
	ended []int
}

func (r *resuming) BeginStep(_ context.Context, ordinal int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begun = append(r.begun, ordinal)
	return nil
}

func (r *resuming) EndStep(_ context.Context, ordinal int, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, ordinal)
	return nil
}

func (r *resuming) NextOrdinal() int { return r.next }

func (r *resuming) slices() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.begun...)
}

var _ = Describe("Reconciliation on resume, in the loop", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	const interval = time.Second
	quick := func(o *run.Options) {
		o.TTL = 3 * interval
		o.Interval = interval
		o.Grace = 5 * time.Second
	}

	// atStepTwo drives the node until its second step's session is in
	// flight, then cancels: the run is live at step 2, exactly as a worker
	// that died there leaves it.
	atStepTwo := func() {
		GinkgoHelper()
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
			fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)},
		)
		runner := f.runner(quick)
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() string { return f.runningStep(ctx, f.nodeRef) }, 30*time.Second, 100*time.Millisecond).
			Should(Equal("implement"), "the second step's session is in flight")
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, interval+10*time.Second).Should(Receive(BeNil()))
		Expect(f.inspect(ctx).GetSummary().GetCurrentStep()).To(Equal("implement"))
	}

	// Scenario: A live run with no machine fails as a lost sandbox
	//   Given a live run at step 2 and no machine for it
	//   When the worker starts
	//   Then the run fails with lost sandbox as evidence
	//   And no new machine is provisioned
	It("A live run with no machine fails as a lost sandbox", func() {
		By("given a live run at step 2 and no machine for it")
		atStepTwo()
		// What the machine backend answers when the record is there and
		// machined does not know the machine; Create is never reached, and
		// opened counts every workspace the loop was handed.
		var opened atomic.Int64
		lost := fmt.Errorf("%w: mill-x is not registered with machined", workspace.ErrLostSandbox)

		By("when the worker starts and claims it")
		f.script(f.quickFixScripts()...)
		worker := f.workerOver(f.runner(quick, func(o *run.Options) {
			o.Workspaces = func(context.Context, workspace.Spec, run.Repository) (run.Workspace, error) {
				opened.Add(1)
				return nil, lost
			}
		}))
		done := startWorker(ctx, worker)
		DeferCleanup(func() {
			worker.Drain()
			Eventually(done, time.Minute).Should(Receive())
		})

		By("then the run fails with lost sandbox as evidence, naming the machine and the ledger's step")
		Eventually(func() millv1.RunStatus { return f.runOf(ctx).GetStatus() }, 30*time.Second, 200*time.Millisecond).
			Should(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		reason := f.failReason(ctx)
		Expect(reason).To(HavePrefix("lost sandbox"))
		Expect(reason).To(ContainSubstring("mill-x"))
		Expect(reason).To(ContainSubstring("the ledger's step is implement"))

		By("and no new machine is provisioned, and the node is held rather than claimed again")
		Expect(opened.Load()).To(Equal(int64(1)), "one attach, which refused; nothing after it")
		node, err := f.client.Node(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetStatus()).To(Equal(skiftv1.Status_STATUS_OPEN), "the work is not done")
		Expect(node.GetHold().GetReason()).To(HavePrefix("lost sandbox"))
		Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the lease was given back")
		Consistently(opened.Load, 3*time.Second, 250*time.Millisecond).Should(Equal(int64(1)))
	})

	It("refuses to drive a failed run on an open node rather than resume it", func() {
		atStepTwo()
		f.script(f.quickFixScripts()...)
		// Fail it the way a lost sandbox does, then claim again by hand.
		err := f.runner(quick, func(o *run.Options) {
			o.Workspaces = func(context.Context, workspace.Spec, run.Repository) (run.Workspace, error) {
				return nil, workspace.ErrLostSandbox
			}
		}).Execute(ctx, f.nodeRef)
		Expect(err).To(MatchError(workspace.ErrLostSandbox))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))

		By("a person releases the hold without restarting, and the next claim is refused, not driven")
		_, err = f.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
			RequestId: "spec-unhold", Project: project,
			Ops: []*skiftv1.PatchOp{{Op: &skiftv1.PatchOp_UnholdNode{UnholdNode: &skiftv1.UnholdNodeOp{Node: f.nodeRef, Reason: "the spec releases it"}}}},
		}))
		Expect(err).NotTo(HaveOccurred())
		err = f.runner(quick).Execute(ctx, f.nodeRef)
		Expect(err).To(MatchError(run.ErrRefused))
		Expect(err.Error()).To(ContainSubstring("mill restart"))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED), "nothing moved it")

		By("and the node is held again, rather than left claimable for the same refusal")
		node, err := f.client.Node(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetHold().GetReason()).To(ContainSubstring("mill restart"),
			"held under the same reason the refusal gave, so a second claim does not repeat it (mill#39)")
	})

	It("refuses to drive a done run on an open node rather than resume it", func() {
		By("given the ledger says the run is done but skift's node was never told (Invariant 11 violated)")
		claimed, err := f.client.Claim(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			NodeId: claimed.Node.GetId(), NodeRef: claimed.Node.GetReference(), ContextId: claimed.Node.GetContextId(),
			Generation: 1, WorkerRef: f.worker, Branch: workspace.BranchFor(claimed.Node.GetReference()),
		}))
		Expect(err).NotTo(HaveOccurred())
		runID := created.Msg.GetRun().GetId()
		_, err = f.store.SetLease(ctx, connect.NewRequest(&millv1.SetLeaseRequest{
			RunId: runID, LeaseId: claimed.Lease.GetId(), WorkerRef: f.worker,
		}))
		Expect(err).NotTo(HaveOccurred())
		_, err = f.store.Done(ctx, connect.NewRequest(&millv1.DoneRequest{
			LeaseId: claimed.Lease.GetId(), RunId: runID,
		}))
		Expect(err).NotTo(HaveOccurred())
		f.expire(ctx, claimed.Lease.GetId())

		By("when the node is claimed again")
		err = f.runner(quick).Execute(ctx, f.nodeRef)

		By("then it is refused rather than resumed, and held so a second claim does not repeat it")
		Expect(err).To(MatchError(run.ErrRefused))
		Expect(err.Error()).To(ContainSubstring("is done but"))
		node, err := f.client.Node(ctx, f.nodeRef)
		Expect(err).NotTo(HaveOccurred())
		Expect(node.GetHold().GetReason()).To(ContainSubstring("is done but"))
		Expect(node.GetStatus()).To(Equal(skiftv1.Status_STATUS_OPEN))
	})

	// Scenario: A live run resumes at its step with a fresh slice
	//   Given a live run at step 2 with a process still in its slice
	//   When the worker starts
	//   Then the slice is stopped and recreated and the run resumes at step 2
	//
	// The stop is the backend's, at attach (internal/workspace/machine); what
	// the loop owes it is to recreate the slice under its own ordinal and to
	// resume at the ledger's step, which is this.
	It("A live run resumes at its step with a fresh slice", func() {
		By("given a live run at step 2 whose workspace was attached with mill-step-2.slice reaped")
		atStepTwo()
		var attached *resuming
		f.wrapWorkspace = func(ws run.Workspace) run.Workspace {
			attached = &resuming{Workspace: ws, next: 2}
			return attached
		}

		By("when the worker resumes it")
		f.script(f.quickFixScripts()...)
		Expect(f.runner(quick).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the slice is recreated under its own ordinal and the run resumes at step 2")
		Expect(attached).NotTo(BeNil())
		Expect(attached.slices()[0]).To(Equal(2), "the first slice begun is the reaped one, recreated")
		detail := f.inspect(ctx)
		Expect(stepRuns(detail, "implement")).To(HaveLen(2), "step 2 was attempted again, from the top")
		Expect(stepRuns(detail, "gather-context")).To(HaveLen(1), "step 1 was not re-run")
		Expect(closedSteps(detail)).To(HaveKey("implement"))
		Expect(attached.slices()).To(Equal([]int{2, 3, 4, 5, 6}), "one fresh slice per step from there on")
	})

	// Scenario: Reconciliation precedes eviction
	//   When the worker starts
	//   Then orphans and leftovers are handled before eviction runs
	It("Reconciliation precedes eviction, and both precede the first claim", func() {
		f.script(f.quickFixScripts()...)
		client, counter := f.counted()
		startup := &recordingStartup{claim: counter.claims.Load}
		worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client }),
			func(o *run.WorkerOptions) { o.Startup = startup })
		done := startWorker(ctx, worker)
		DeferCleanup(func() {
			worker.Drain()
			Eventually(done, time.Minute).Should(Receive())
		})

		Eventually(func() *millv1.Run { return f.runOfRef(ctx, f.nodeRef) }, 30*time.Second, 200*time.Millisecond).
			ShouldNot(BeNil(), "the node was claimed")
		Expect(startup.seen()).To(Equal([]string{"reconcile (claims so far: 0)", "evict (claims so far: 0)"}))
	})

	// Scenario: The sweep precedes the host's pass, and both precede the first claim
	//   Given a live run whose node was dropped
	//   When the worker starts
	//   Then the run is failed before the host's reconciliation runs, and nothing was claimed
	It("The sweep precedes the host's pass, and both precede the first claim", func() {
		By("given a live run whose node was dropped, a lease TTL ago")
		atStepTwo()
		first := f.runOf(ctx)
		f.drop(ctx, f.nodeRef)
		time.Sleep(3*interval + time.Second)

		By("when the worker starts")
		client, counter := f.counted()
		startup := &recordingStartup{claim: counter.claims.Load, also: func() string {
			return f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus().String()
		}}
		worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client }),
			func(o *run.WorkerOptions) { o.Startup = startup })
		done := startWorker(ctx, worker)
		DeferCleanup(func() {
			worker.Drain()
			Eventually(done, time.Minute).Should(Receive())
		})

		By("then the run is failed before the host's reconciliation runs, and nothing was claimed")
		Eventually(startup.seen, 30*time.Second, 200*time.Millisecond).Should(HaveLen(2))
		Expect(startup.seen()).To(Equal([]string{
			"reconcile (claims so far: 0) RUN_STATUS_FAILED",
			"evict (claims so far: 0) RUN_STATUS_FAILED",
		}))
	})

	// Scenario: A worker with no startup sweeps at start and again on the interval
	//   Given a worker on the local backend
	//   When a run is stranded after it has started
	//   Then the run is failed once the sweep interval elapses, and not before
	It("A worker with no startup sweeps at start and again on the interval", func() {
		By("given a worker on the local backend, idle")
		// A label nothing carries keeps the worker from claiming the node
		// the spec is about to strand: a stranded run is one nobody will
		// claim, and the worker sweeping it must not be the one racing to.
		worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) {
			o.SweepInterval = interval
			o.Labels = []string{"never-filed"}
		})
		done := startWorker(ctx, worker)
		DeferCleanup(func() {
			worker.Drain()
			Eventually(done, time.Minute).Should(Receive())
		})

		By("when a run is stranded after it has started")
		stray := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		By("then the run is failed once the sweep interval and the lease TTL have elapsed")
		status := func() millv1.RunStatus {
			return f.inspectRun(ctx, stray.GetId()).GetSummary().GetRun().GetStatus()
		}
		Consistently(status, 2*interval, 200*time.Millisecond).Should(Equal(millv1.RunStatus_RUN_STATUS_SELECTING),
			"a run written to inside one lease TTL is left alone")
		Eventually(status, 30*time.Second, 200*time.Millisecond).Should(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
	})

	It("A worker sweeps no more often than its interval", func() {
		worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) {
			o.SweepInterval = time.Hour
			o.Labels = []string{"never-filed"}
		})
		done := startWorker(ctx, worker)
		DeferCleanup(func() {
			worker.Drain()
			Eventually(done, time.Minute).Should(Receive())
		})

		stray := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		Consistently(func() millv1.RunStatus {
			return f.inspectRun(ctx, stray.GetId()).GetSummary().GetRun().GetStatus()
		}, 3*interval+2*time.Second, 250*time.Millisecond).Should(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
	})

	It("does not claim at all when reconciliation cannot run", func() {
		startup := &recordingStartup{claim: func() int64 { return 0 }, fail: errors.New("machinectl did not answer")}
		worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Startup = startup })

		err := worker.Run(ctx)

		Expect(err).To(MatchError(ContainSubstring("machinectl did not answer")))
		Expect(startup.seen()).To(HaveLen(1), "eviction did not run either")
		Consistently(func() *millv1.Run { return f.runOfRef(ctx, f.nodeRef) }, 2*time.Second, 250*time.Millisecond).
			Should(BeNil())
	})

	It("reads the ledger's live runs with their steps, and records a pass", func() {
		atStepTwo()
		ledger := run.NewLedger(f.store, "spec@host", nil)

		live, err := ledger.LiveRuns(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(live).To(Equal([]machine.LiveRun{{RunID: f.runOf(ctx).GetId(), NodeRef: f.nodeRef, Step: "implement"}}))

		started := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
		Expect(ledger.RecordReconciliation(ctx, machine.Report{StartedAt: started, Findings: []machine.Finding{
			{Kind: machine.FindingOrphan, Name: "mill-x", Action: "stopped; run directory removed", Detail: "unit mill-machine@x.service"},
		}})).To(Succeed())
		var findings string
		var workerRef string
		Expect(f.db.QueryRow(ctx, `SELECT worker_ref, findings::text FROM reconciliations ORDER BY id DESC LIMIT 1`).
			Scan(&workerRef, &findings)).To(Succeed())
		Expect(workerRef).To(Equal("spec@host"))
		Expect(findings).To(ContainSubstring(`"name": "mill-x"`))
		Expect(findings).To(ContainSubstring(`"kind": "orphan"`))

		By("and a run that has ended is not live")
		f.script(f.quickFixScripts()...)
		Expect(f.runner(quick).Execute(ctx, f.nodeRef)).To(Succeed())
		live, err = ledger.LiveRuns(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(live).To(BeEmpty())
	})

	It("logs a workflow row that will not decode and proceeds with the runs it can read", func() {
		atStepTwo()
		// The run's workflow, as a mill whose schema had a `model` field
		// would have stored it: the bytes no longer decode under this one.
		workflowID := f.runOf(ctx).GetWorkflowId()
		_, err := f.db.Exec(ctx, `UPDATE workflows SET definition = definition || '{"model": "opus"}'::jsonb WHERE id = $1`, workflowID)
		Expect(err).NotTo(HaveOccurred())

		var logged bytes.Buffer
		ledger := run.NewLedger(f.store, "spec@host", slog.New(slog.NewTextHandler(&logged, nil)))

		live, err := ledger.LiveRuns(ctx)
		Expect(err).NotTo(HaveOccurred(), "the startup goes on")
		Expect(live).To(Equal([]machine.LiveRun{{RunID: f.runOf(ctx).GetId(), NodeRef: f.nodeRef}}),
			"the run is still live and still listed; its step is unknown without its definition")
		Expect(logged.String()).To(ContainSubstring("workflow row does not decode"))
		Expect(logged.String()).To(ContainSubstring("workflow=" + workflowID))
		Expect(logged.String()).To(ContainSubstring("field=model"))
	})
})

// failReason is what the audit trail says the run failed for.
func (f *fixture) failReason(ctx context.Context) string {
	GinkgoHelper()
	res, err := f.store.History(ctx, connect.NewRequest(&millv1.HistoryRequest{NodeRef: f.nodeRef}))
	Expect(err).NotTo(HaveOccurred())
	for _, event := range res.Msg.GetEvents() {
		if event.GetOp() == "failed" {
			return event.GetAfter().GetFields()["reason"].GetStringValue()
		}
	}
	return ""
}
