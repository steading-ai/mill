package run_test

import (
	"context"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace/machine"
)

// docs/ledger.md §Sweep: the ledger reconciled against skift. A run whose
// node was closed underneath it — dropped, or completed by someone else —
// is never claimed again, so nothing inside a claim can end it; the sweep
// does, and gives back what the run held.

var _ = Describe("The stranded-run sweep", Label(testsupport.DBLabel), func() {
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
	// aged is a clock a lease TTL and more past every write the spec made,
	// so the sweep's age guard — a run touched inside one TTL is left for
	// its process — does not stand between the spec and the sweep.
	aged := func(o *run.Options) {
		o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	}
	sweep := func(options ...func(*run.Options)) machine.Report {
		GinkgoHelper()
		report, err := f.runner(append([]func(*run.Options){quick, aged}, options...)...).Sweep(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		return report
	}
	stranded := func(report machine.Report) []machine.Finding {
		return report.Of(run.FindingStranded)
	}
	lastPass := func() string {
		GinkgoHelper()
		var findings string
		Expect(f.db.QueryRow(ctx, `SELECT findings::text FROM reconciliations ORDER BY id DESC LIMIT 1`).
			Scan(&findings)).To(Succeed())
		return findings
	}

	// atStepTwo drives the node until its second step's session is in
	// flight, then cancels: the run is `running` at step 2 with its lease
	// released, exactly as a worker that was stopped there leaves it — and
	// with no live lease, skift admits a drop.
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
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
	}

	// parked drives the node into the circuit breaker: `implement` names no
	// on_failure, so its first failure files a park gate and releases.
	parked := func() {
		GinkgoHelper()
		f.script(ok("context gathered, then stuck"), failed("I cannot make sense of this"))
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
	}

	// Scenario: A running run whose node was dropped after its lease was released fails
	//   Given a run at step 2 whose lease was released
	//   And its node dropped
	//   When the worker sweeps
	//   Then the run is failed with the drop as the reason, its workspace destroyed and its branch closed
	It("A running run whose node was dropped after its lease was released fails", func() {
		By("given a run at step 2 whose lease was released, and its node dropped")
		atStepTwo()
		first := f.runOf(ctx)
		f.drop(ctx, f.nodeRef)
		Expect(f.statusOf(ctx, f.nodeRef)).To(Equal(skiftv1.Status_STATUS_DROPPED))

		By("when the worker sweeps")
		report := sweep()

		By("then the run is failed with the drop as the reason")
		after := f.inspectRun(ctx, first.GetId()).GetSummary()
		Expect(after.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		Expect(f.failReason(ctx)).To(HavePrefix("node dropped underneath the run"))
		Expect(f.openStepRun(ctx)).To(BeNil(), "no attempt is left open")

		By("and its workspace is destroyed and its branch closed on the forge")
		Expect(after.GetWorkspace().GetState()).To(Equal(millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED))
		Expect(f.forge.branchesDeleted()).To(ContainElement(first.GetBranch()))
		Expect(f.forge.closingComments()).To(HaveLen(1))
		Expect(f.forge.closingComments()[0]).To(ContainSubstring("node dropped underneath the run"))

		By("and the pass records a stranded finding")
		Expect(stranded(report)).To(HaveLen(1))
		Expect(stranded(report)[0].Name).To(Equal(first.GetId()))
		Expect(stranded(report)[0].Action).To(Equal("failed"))
		Expect(stranded(report)[0].Detail).To(ContainSubstring("the ledger's step was implement"))
		Expect(lastPass()).To(ContainSubstring(`"kind": "stranded"`))
	})

	// Scenario: A selecting run whose node was dropped fails with nothing to close on the forge
	//   Given a run that never chose a workflow, holding no lease
	//   And its node dropped
	//   When the worker sweeps
	//   Then the run is failed and the forge is not touched
	It("A selecting run whose node was dropped fails with nothing to close on the forge", func() {
		By("given a run that never chose a workflow, and its node dropped")
		first := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		By("when the worker sweeps")
		report := sweep()

		By("then the run is failed and the forge is not touched")
		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		Expect(f.failReason(ctx)).To(HavePrefix("node dropped underneath the run"))
		Expect(f.forge.branchesDeleted()).To(BeEmpty())
		Expect(f.forge.closingComments()).To(BeEmpty())
		Expect(stranded(report)).To(HaveLen(1))
		Expect(stranded(report)[0].Detail).To(ContainSubstring("(selecting)"))
	})

	// Scenario: A parked run whose node was dropped fails
	//   Given a run parked with its gate unanswered
	//   And its node dropped
	//   When the worker sweeps
	//   Then the run is failed at the step it was parked on
	It("A parked run whose node was dropped fails", func() {
		By("given a parked run, and its node dropped")
		parked()
		first := f.runOf(ctx)
		f.drop(ctx, f.nodeRef)

		By("when the worker sweeps")
		report := sweep()

		By("then the run is failed at the step it was parked on")
		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		Expect(stranded(report)).To(HaveLen(1))
		Expect(stranded(report)[0].Detail).To(ContainSubstring("the ledger's step was implement"))
	})

	// Scenario: A parked run whose node is open and blocked by its gate survives the sweep
	//   Given a run parked with its gate unanswered
	//   When the worker sweeps
	//   Then the run is still parked and its workspace retained
	It("A parked run whose node is open and blocked by its gate survives the sweep", func() {
		By("given a run parked with its gate unanswered")
		parked()
		first := f.runOf(ctx)

		By("when the worker sweeps")
		report := sweep()

		By("then the run is still parked and its workspace retained")
		after := f.inspectRun(ctx, first.GetId()).GetSummary()
		Expect(after.GetRun().GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(after.GetWorkspace().GetState()).NotTo(Equal(millv1.WorkspaceState_WORKSPACE_STATE_DESTROYED))
		Expect(report.Findings).To(BeEmpty())
		Expect(f.forge.branchesDeleted()).To(BeEmpty())
	})

	// Scenario: A node completed by a person underneath a run fails the run
	//   Given a run at step 2 whose lease was released
	//   And its node completed by a person
	//   When the worker sweeps
	//   Then the run is failed as completed outside the run
	It("A node completed by a person underneath a run fails the run", func() {
		By("given a run at step 2 whose lease was released, and its node completed by a person")
		atStepTwo()
		first := f.runOf(ctx)
		f.complete(ctx, f.nodeRef)

		By("when the worker sweeps")
		report := sweep()

		By("then the run is failed as completed outside the run")
		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		Expect(f.failReason(ctx)).To(HavePrefix("node completed outside the run"))
		Expect(stranded(report)).To(HaveLen(1))
		Expect(stranded(report)[0].Action).To(Equal("failed"))
	})

	// Scenario: A completion the store missed is caught up
	//   Given a run whose completing patch landed and whose store write did not
	//   When the worker sweeps
	//   Then the run is marked done and nothing is closed on the forge
	It("A completion the store missed is caught up", func() {
		By("given a run whose completing patch landed and whose store write did not")
		atStepTwo()
		first := f.runOf(ctx)
		_, err := f.store.SetCompleteRequestId(ctx, connect.NewRequest(&millv1.SetCompleteRequestIdRequest{
			LeaseId: first.GetLeaseId(), RunId: first.GetId(), RequestId: "mill-" + first.GetId() + "-complete-1",
		}))
		Expect(err).NotTo(HaveOccurred())
		f.complete(ctx, f.nodeRef)

		By("when the worker sweeps")
		report := sweep()

		By("then the run is marked done and nothing is closed on the forge")
		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
		Expect(stranded(report)).To(HaveLen(1))
		Expect(stranded(report)[0].Action).To(Equal("done"))
		Expect(f.forge.branchesDeleted()).To(BeEmpty())
		Expect(f.forge.closingComments()).To(BeEmpty())
	})

	// Scenario: A run another worker holds is not this worker's to sweep
	//   Given a stranded run recorded under another worker
	//   When this worker sweeps
	//   Then the run is untouched
	It("A run another worker holds is not this worker's to sweep", func() {
		first := f.selectingRun(ctx, "other@elsewhere")
		f.drop(ctx, f.nodeRef)

		report := sweep()

		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
		Expect(report.Findings).To(BeEmpty())
	})

	// Scenario: A run touched inside one lease TTL is left for its process
	//   Given a stranded run written to moments ago
	//   When the worker sweeps under its ordinary clock
	//   Then the run is untouched until a TTL has passed
	It("A run touched inside one lease TTL is left for its process", func() {
		first := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		report, err := f.runner(func(o *run.Options) { o.TTL = time.Hour }).Sweep(ctx, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
		Expect(report.Findings).To(BeEmpty())
	})

	// Scenario: A run this process is driving is never swept
	//   Given a stranded run whose node this process reports it is driving
	//   When the worker sweeps
	//   Then the run is untouched
	It("A run this process is driving is never swept", func() {
		first := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		report, err := f.runner(quick, aged).Sweep(ctx, func(nodeID string) bool { return nodeID == first.GetNodeId() })
		Expect(err).NotTo(HaveOccurred())

		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
		Expect(report.Findings).To(BeEmpty())
	})

	// Scenario: A node skift cannot read is a failure finding, not a stop
	//   Given a live run on a node skift does not know beside a stranded one
	//   When the worker sweeps
	//   Then the unreadable one is a failure finding and the stranded one is still failed
	It("A node skift cannot read is a failure finding, not a stop", func() {
		By("given a live run on a node skift does not know, beside a stranded one")
		res, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
			NodeId: "01SPECNOWHERE00000000000000", NodeRef: project + "#404", ContextId: "nowhere",
			Generation: 1, WorkerRef: f.worker, Branch: "mill/nowhere",
		}))
		Expect(err).NotTo(HaveOccurred())
		nowhere := res.Msg.GetRun()
		stray := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		By("when the worker sweeps")
		report := sweep()

		By("then the unreadable one is a failure finding and the stranded one is still failed")
		Expect(report.Of(machine.FindingFailure)).To(HaveLen(1))
		Expect(report.Of(machine.FindingFailure)[0].Name).To(Equal(nowhere.GetId()))
		Expect(f.inspectRun(ctx, nowhere.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
		Expect(f.inspectRun(ctx, stray.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
	})

	// Scenario: mill run on a dropped node resolves its run instead of claiming
	//   Given a run at step 2 whose node was dropped
	//   When mill run is invoked for the node
	//   Then the run is failed and no lease is taken
	It("mill run on a dropped node resolves its run instead of claiming", func() {
		atStepTwo()
		first := f.runOf(ctx)
		f.drop(ctx, f.nodeRef)

		Expect(f.runner(quick).Execute(ctx, f.nodeRef)).To(Succeed())

		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_FAILED))
		Expect(f.failReason(ctx)).To(HavePrefix("node dropped underneath the run"))
		Expect(f.leaseOf(ctx)).To(BeNil(), "no lease was taken")
	})

	// Scenario: mill restart refuses a node that is not open
	//   Given a node that was dropped
	//   When mill restart is invoked for it
	//   Then it is refused as not open and no generation is created
	It("mill restart refuses a node that is not open", func() {
		first := f.selectingRun(ctx, f.worker)
		f.drop(ctx, f.nodeRef)

		_, err := f.runner().Restart(ctx, f.nodeRef, "restart")

		Expect(err).To(MatchError(run.ErrRefused))
		Expect(err.Error()).To(ContainSubstring("not open"))
		Expect(f.inspectRun(ctx, first.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING), "nothing was abandoned")
	})
})
