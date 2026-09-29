package run_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// The three ways the first end-to-end run could not be recovered from
// (mill#35). Each is a resume the algorithm in docs/ledger.md §Resume
// specifies and the loop did not do: a run that died before it chose a
// workflow, a restart that needed a lease nothing held any more, and an
// attempt sent to the harness as a resume of a session that was never
// started.
var _ = Describe("Resume after a run died early", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and the run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	// died is a run that got no further than its workspace: created,
	// holding the lease of the claim that created it, and dead before the
	// workflow was chosen. The lease it holds is released on the way out,
	// which is the state a crash leaves once skift's TTL has run down —
	// the ledger names a lease skift no longer knows about.
	died := func() *millv1.Run {
		GinkgoHelper()
		var calls atomic.Int64
		inner := f.workspaces()
		err := f.runner(func(o *run.Options) {
			o.Workspaces = func(ctx context.Context, spec workspace.Spec, repo run.Repository) (run.Workspace, error) {
				if calls.Add(1) == 1 {
					return nil, errors.New("the worker died making the workspace")
				}
				return inner(ctx, spec, repo)
			}
		}).Execute(ctx, f.nodeRef)
		Expect(err).To(HaveOccurred(), "the run died before it chose a workflow")

		run := f.runOf(ctx)
		Expect(run.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_SELECTING))
		Expect(run.GetWorkflowId()).To(BeEmpty())
		Expect(run.GetLeaseId()).NotTo(BeEmpty(), "the ledger still names the dead worker's lease")
		Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "skift's lease is gone")
		return run
	}

	It("A run that never chose a workflow can still be resumed", func() {
		By("given a run whose status is selecting and whose lease has expired")
		dead := died()

		By("when mill run is invoked for the node")
		f.script(f.quickFixScripts()...)
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the run is claimed under a new lease")
		resumed := f.runOf(ctx)
		Expect(resumed.GetId()).To(Equal(dead.GetId()), "one run, not a second")
		Expect(resumed.GetGeneration()).To(Equal(int64(1)))

		detail := f.inspect(ctx)
		Expect(leasesIn(detail)).To(HaveLen(1), "every attempt ran under the claim that made them")
		Expect(leasesIn(detail)).NotTo(ContainElement(dead.GetLeaseId()))
		Expect(append(leasesIn(detail), dead.GetLeaseId())).To(HaveLen(2),
			"the ledger shows one run and two lease ids")

		By("and a workflow is selected and the first step starts")
		Expect(resumed.GetWorkflowId()).NotTo(BeEmpty())
		Expect(closedSteps(detail)).To(HaveKey("select"))
		Expect(closedSteps(detail)).To(HaveKey("gather-context"))
	})

	It("Restart does not need the dead lease", func() {
		By("given a parked run whose skift lease has expired")
		// The ledger nulls a run's lease when the circuit breaker parks it,
		// so a stale lease on a stopped run is always the one a claim left
		// behind when its worker died — which is the run a human restarts,
		// and the run the restart of 2026-09-08 could not get past.
		dead := died()
		out := &bytes.Buffer{}

		By("when mill restart is invoked for the node")
		next, err := f.runner(func(o *run.Options) { o.Out = out }).Restart(ctx, f.nodeRef, "restart")
		Expect(err).NotTo(HaveOccurred())

		By("then generation 2 is created")
		Expect(next.GetGeneration()).To(Equal(int64(2)))
		Expect(f.runOf(ctx).GetId()).To(Equal(next.GetId()))

		By("and the old generation is abandoned")
		Expect(f.inspectRun(ctx, dead.GetId()).GetSummary().GetRun().GetStatus()).
			To(Equal(millv1.RunStatus_RUN_STATUS_ABANDONED))
		Expect(out.String()).To(ContainSubstring(dead.GetLeaseId()))
		Expect(out.String()).To(ContainSubstring("skipped the release"))
	})

	It("A session that never started is started, not resumed", func() {
		By("given a step whose only attempt failed before the harness produced any event")
		first := f.cancelledTurn(ctx, fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})
		Expect(sessionsOf(f.inspect(ctx), "gather-context")).To(ContainElement(first))
		Expect(startedSessions(f.inspect(ctx))).To(BeEmpty(),
			"nothing was said in the session, so the ledger holds no conversation for it")

		By("when the run resumes at that step")
		f.script(f.quickFixScripts()...)
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the harness receives a Start request for the session id")
		calls := f.harness.Calls()
		Expect(calls).NotTo(BeEmpty())
		Expect(calls[0].Session.ID).To(Equal(first))
		Expect(calls[0].Verb).To(Equal("start"))
	})

	It("A session that was started and aborted mid-turn is resumed", func() {
		By("given a step whose attempt was aborted after the harness had spoken")
		first := f.cancelledTurn(ctx, fake.Script{
			Events:    []*harnessv1.Event{fake.Text("turn-1", "reading the work order")},
			Hold:      true,
			Cancelled: fake.Terminal("stopped", nil, nil),
		})
		Expect(startedSessions(f.inspect(ctx))).To(ContainElement(first))

		By("when the run resumes at that step")
		f.script(f.quickFixScripts()...)
		Expect(f.runner().Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the harness receives a Resume request for the session id")
		calls := f.harness.Calls()
		Expect(calls).NotTo(BeEmpty())
		Expect(calls[0].Session.ID).To(Equal(first))
		Expect(calls[0].Verb).To(Equal("resume"))
		Expect(calls[0].Prompt.Text).NotTo(ContainSubstring("## What the last attempt failed on"),
			"gather-context is not a revise target and follows no failed attempt: it owes nothing (mill#100)")
	})

	It("A session aborted mid-turn on a revise target's attempt is resumed with the revise's finding", func() {
		By("given a feature run whose review attempt 1 has asked for changes, implement's second attempt aborted after the harness had spoken")
		f.pushWorkflowFrom(ctx, featureDir)
		writesPlan := func(harness.Session) {
			defer GinkgoRecover()
			Expect(f.workspace.Write(context.Background(), planPath(f.nodeRef),
				[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)))).To(Succeed())
		}
		const interval = time.Second
		const grace = 5 * time.Second
		f.script(
			ok("context gathered"),
			ok("explored the problem"),
			fake.Script{Terminal: fake.Terminal("planned", nil, usage(10)), Do: writesPlan},
			fake.Script{Terminal: fake.Terminal("", approves("AC-1", "the plan names its proof"), usage(5))},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("simplified", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("", revises("AC-2", "needs another pass"), usage(5))},
			fake.Script{
				Events:    []*harnessv1.Event{fake.Text("turn-1", "reading the finding")},
				Hold:      true,
				Cancelled: fake.Terminal("stopped", nil, nil),
			},
		)

		runner := f.runner(func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
			o.TTL = 3 * interval
			o.Interval = interval
			o.Grace = grace
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()
		Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
			Should(BeNumerically(">=", 10), "implement's second attempt, answering the revise, has spoken")
		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, interval+grace).Should(Receive(BeNil()))

		implementSessions := sessionsOf(f.inspect(ctx), "implement")
		Expect(implementSessions).To(HaveLen(2), "round 1, superseded by the revise, and round 2, aborted mid-turn")
		aborted := implementSessions[1]
		Expect(startedSessions(f.inspect(ctx))).To(ContainElement(aborted))

		By("when the run resumes at implement")
		f.script(
			fake.Script{Terminal: fake.Terminal("the change is made again", nil, usage(20)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("simplified again", nil, usage(10)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("committed again", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("opened the pull request again", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("", approves("AC-2", "the fix addresses the finding"), usage(5))},
		)
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = "feature"
			o.WorkflowDir = mustAbs(featureDir)
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the harness receives a Resume request for the aborted session, carrying review's finding")
		calls := f.harness.Calls()
		Expect(calls).NotTo(BeEmpty())
		Expect(calls[0].Session.ID).To(Equal(aborted))
		Expect(calls[0].Verb).To(Equal("resume"))
		Expect(calls[0].Prompt.Text).To(ContainSubstring("needs another pass"))
	})
})

// cancelledTurn runs the node until its first session is in flight, then
// cancels it: the attempt is aborted mid-turn, exactly as a worker killed
// under a session leaves it, and the run stays resumable at that step. It
// returns the session id the attempt ran in.
func (f *fixture) cancelledTurn(ctx context.Context, script fake.Script) string {
	GinkgoHelper()
	const interval = time.Second
	const grace = 5 * time.Second

	f.script(script)
	runner := f.runner(func(o *run.Options) {
		o.TTL = 3 * interval
		o.Interval = interval
		o.Grace = grace
	})
	done := make(chan error, 1)
	go func() { done <- runner.Execute(ctx, f.nodeRef) }()
	Eventually(func() int { return len(f.harness.Calls()) }, 30*time.Second, 100*time.Millisecond).
		Should(BeNumerically(">", 0), "the session is in flight")
	Expect(f.cancelRun(ctx)).To(Succeed())
	Eventually(done, interval+grace).Should(Receive(BeNil()))

	sessions := sessionsOf(f.inspect(ctx), "gather-context")
	Expect(sessions).To(HaveLen(1))
	return sessions[0]
}

// leasesIn is every distinct lease the run's attempts were made under.
func leasesIn(detail *millv1.InspectResponse) []string {
	var leases []string
	seen := map[string]bool{}
	for _, d := range detail.GetSteps() {
		lease := d.GetStepRun().GetLeaseId()
		if lease == "" || seen[lease] {
			continue
		}
		seen[lease] = true
		leases = append(leases, lease)
	}
	return leases
}

// startedSessions is every session the ledger says the harness has spoken
// in, which is what decides between Start and Resume.
func startedSessions(detail *millv1.InspectResponse) []string {
	var ids []string
	for _, d := range detail.GetSteps() {
		if d.GetStepRun().GetSessionStarted() {
			ids = append(ids, d.GetStepRun().GetSessionId())
		}
	}
	return ids
}
