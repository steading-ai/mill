package run_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/fake"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/testsupport"
)

var _ = Describe("The worker", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		// Not the node's own SpecContext: Ginkgo cancels that when the node
		// it was given to returns, and a run outlives this one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
	})

	// A heartbeat short enough that a spec waiting on one is not waiting on
	// five minutes. The TTL is three intervals, which is the bound the
	// design puts on the renewal.
	const interval = time.Second
	const ttl = 3 * interval

	// quick is the run loop under a fast heartbeat, which is what makes a
	// spec's stop or fence land inside a spec's patience.
	quick := func(o *run.Options) {
		o.TTL = ttl
		o.Interval = interval
		o.Grace = 5 * time.Second
	}

	Describe("Claiming", func() {
		It("The daemon is the loop, not a scheduler", func() {
			By("given no ready nodes")
			// The only open node is one a person must complete, and the
			// only other is blocked by it. A worker sees nothing.
			blocker := f.userNode(ctx, "a decision only a person can make", "Say yes or no.")
			node := f.createNode(ctx, &daggerv1.CreateNodeOp{
				Title: "the work behind the decision", Body: "Change the README's first line.",
				BlockedBy: []string{blocker},
			})
			f.script(f.quickFixScripts()...)

			client, counter := f.counted()
			worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Dagger = client }),
				func(o *run.WorkerOptions) { o.Wait = 30 * time.Second })
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Stop()
				Eventually(done, time.Minute).Should(Receive())
			})
			Consistently(func() *millv1.Run { return f.runOfRef(ctx, node) }, 2*time.Second, 250*time.Millisecond).
				Should(BeNil(), "nothing is claimed while nothing is ready")

			By("when a node becomes ready by its predecessor completing")
			f.complete(ctx, blocker)

			By("then work claims it without any event subscription, within the ClaimNext wait")
			// Within the wait, not after it: the worker is still inside the
			// thirty-second ClaimNext it began before the node existed, so
			// a claim landing at all is the wait doing the work.
			Eventually(func() *millv1.Run { return f.runOfRef(ctx, node) }, 5*time.Second, 100*time.Millisecond).
				ShouldNot(BeNil())
			Expect(counter.events.Load()).To(BeZero(), "nothing subscribed to dagger's change stream")
			Expect(counter.claims.Load()).To(BeNumerically("<=", 2),
				"one blocking claim, not a poll of its own")
		})

		It("blocks with no ready work and returns within a second of a node becoming ready", func() {
			blocker := f.userNode(ctx, "a decision only a person can make", "Say yes or no.")
			node := f.createNode(ctx, &daggerv1.CreateNodeOp{
				Title: "the work behind the decision", Body: "Change the README's first line.",
				BlockedBy: []string{blocker},
			})

			claimed := make(chan *dagger.Claimed, 1)
			go func() {
				defer GinkgoRecover()
				got, err := f.client.ClaimNext(ctx, nil, 30*time.Second)
				Expect(err).NotTo(HaveOccurred())
				claimed <- got
			}()
			Consistently(claimed, 2*time.Second, 250*time.Millisecond).ShouldNot(Receive(),
				"the claim blocks rather than returning empty")

			ready := time.Now()
			f.complete(ctx, blocker)

			// Dagger polls its own ready set at most a second apart inside
			// the wait, so the bound is that second plus the round trip.
			var got *dagger.Claimed
			Eventually(claimed, 2*time.Second, 50*time.Millisecond).Should(Receive(&got))
			Expect(got.Node.GetReference()).To(Equal(node))
			Expect(time.Since(ready)).To(BeNumerically("<", 2*time.Second))
		})

		It("stops without waiting out the claim it is blocked in", func() {
			// Nothing is ready, so the worker is inside a claim with most
			// of its wait left. A stop that had to wait that out would be a
			// unit that takes a minute to restart.
			f.userNode(ctx, "a decision only a person can make", "Say yes or no.")
			worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Wait = 5 * time.Minute })
			done := startWorker(ctx, worker)

			Eventually(func() int { return len(f.harness.Calls()) }, time.Second, 50*time.Millisecond).
				Should(BeZero(), "there is nothing to claim")
			worker.Stop()
			Eventually(done, 10*time.Second).Should(Receive(BeNil()))
		})

		It("claims nothing outside its project and label filters", func() {
			mine := f.labelledNode(ctx, "work this mill takes", "Change the README's first line.", "mill")
			theirs := f.labelledNode(ctx, "work for somebody else", "Not this mill's.")
			f.script(f.quickFixScripts()...)

			worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Labels = []string{"mill"} })
			done := startWorker(ctx, worker)
			Eventually(func() daggerv1.Status { return f.statusOf(ctx, mine) }, 2*time.Minute, 200*time.Millisecond).
				Should(Equal(daggerv1.Status_STATUS_DONE))
			worker.Stop()
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			Expect(f.statusOf(ctx, theirs)).To(Equal(daggerv1.Status_STATUS_OPEN))
			Expect(f.runOfRef(ctx, theirs)).To(BeNil(), "the unlabelled node was never claimed")
		})
	})

	Describe("Concurrency", func() {
		It("completes two ready nodes in sequence at concurrency one, and exits only on signal", func() {
			first := f.node(ctx, "the first fix", "Change the README's first line.")
			second := f.node(ctx, "the second fix", "Change the README's second line.")
			f.script(f.quickFixScripts()...)

			worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Concurrency = 1 })
			done := startWorker(ctx, worker)

			Eventually(func() []daggerv1.Status {
				return []daggerv1.Status{f.statusOf(ctx, first), f.statusOf(ctx, second)}
			}, 3*time.Minute, 200*time.Millisecond).Should(Equal([]daggerv1.Status{
				daggerv1.Status_STATUS_DONE, daggerv1.Status_STATUS_DONE,
			}))
			Consistently(done, time.Second).ShouldNot(Receive(), "the loop asks for more work rather than ending")

			worker.Stop()
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			runs := []*millv1.Run{f.runOfRef(ctx, first), f.runOfRef(ctx, second)}
			for _, r := range runs {
				Expect(r.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))
			}
			// One slot means one run at a time: whichever was created
			// second cannot have been created before the first had ended.
			earlier, later := runs[0], runs[1]
			if later.GetCreatedAt().AsTime().Before(earlier.GetCreatedAt().AsTime()) {
				earlier, later = later, earlier
			}
			Expect(later.GetCreatedAt().AsTime()).To(BeTemporally(">=", earlier.GetEndedAt().AsTime()),
				"the runs did not overlap")
		})
	})

	Describe("Stopping", func() {
		It("Stop is resumable", func() {
			By("given work is executing the implement step of node N")
			node := f.node(ctx, "a fix that is interrupted", "Change the README's first line.")
			f.script(
				ok("context gathered"),
				fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)},
			)
			worker := f.workerOver(f.runner(quick))
			done := startWorker(ctx, worker)
			Eventually(func() string { return f.runningStep(ctx, node) }, time.Minute, 100*time.Millisecond).
				Should(Equal("implement"))
			interrupted := f.openStepRun(ctx)

			By("when SIGTERM arrives")
			// Stop is what the process's signal handler calls; the handler
			// itself is specified in internal/cli, where the signals are.
			worker.Stop()
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			By("then N's lease is released preempted/shutdown")
			reason, detail := f.releasedLease(ctx)
			Expect(reason).To(Equal("preempted"))
			Expect(detail).To(Equal("shutdown"))
			Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the node is claimable again")
			Expect(f.stepRunStatus(ctx, interrupted.GetId())).
				To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))

			By("and the next work claims N and starts attempt 2 of implement in the same session")
			f.script(f.quickFixScripts()...)
			next := f.workerOver(f.runner(quick))
			resumed := startWorker(ctx, next)
			Eventually(func() daggerv1.Status { return f.statusOf(ctx, node) }, 3*time.Minute, 200*time.Millisecond).
				Should(Equal(daggerv1.Status_STATUS_DONE))
			next.Stop()
			Eventually(resumed, time.Minute).Should(Receive(BeNil()))

			detailAfter := f.inspect(ctx)
			attempts := stepRuns(detailAfter, "implement")
			Expect(attempts).To(HaveLen(2))
			Expect(attempts[1].GetAttempt()).To(Equal(int64(2)))
			Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
			Expect(sessionsOf(detailAfter, "implement")).
				To(Equal([]string{interrupted.GetSessionId(), interrupted.GetSessionId()}))
		})

		It("kills the sessions in flight and exits non-zero when it is stopped a second time", func() {
			f.node(ctx, "a session that will not go", "Change the README's first line.")
			// A session that ignores the interrupt is what the grace window
			// exists for: the adapter's Cancel does not return until the
			// process group is gone, and here it is gone only when the kill
			// arrives. The real adapter's escalation to SIGKILL is
			// specified in internal/harness/claude.
			stubborn := newStubborn()
			worker := f.workerOver(f.runner(quick, withHarness(stubborn)))

			killable, kill := context.WithCancel(ctx)
			DeferCleanup(kill)
			done := startWorker(killable, worker)
			Eventually(stubborn.started, time.Minute).Should(BeClosed())

			By("the first stop asks, and the session does not go")
			worker.Stop()
			Consistently(done, 2*time.Second).ShouldNot(Receive())

			By("the second kills it")
			kill()
			var err error
			Eventually(done, time.Minute).Should(Receive(&err))
			Expect(err).To(MatchError(run.ErrKilled))
			Expect(stubborn.cancelled.Load()).To(BeTrue(), "the session was cancelled, not left behind")
		})
	})

	Describe("Losing a run", func() {
		It("skips a node it was fenced out of and goes back to claiming", func() {
			taken := f.node(ctx, "a node taken by somebody else", "Change the README's first line.")
			// Mid-step, another principal takes the lease. The next
			// heartbeat is refused, which is the whole of the worker's
			// notice: it abandons the run locally and writes nothing more.
			f.script(fake.Script{Hold: true, Do: func(harness.Session) {
				defer GinkgoRecover()
				f.expire(ctx, f.leaseOf(ctx).GetId())
				f.reclaim(ctx)
			}})

			client, counter := f.counted()
			worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Dagger = client }))
			done := startWorker(ctx, worker)

			// A second claim is the worker carrying on: it asked for more
			// work rather than ending on somebody else's node.
			Eventually(func() int64 { return counter.claims.Load() }, time.Minute, 100*time.Millisecond).
				Should(BeNumerically(">=", 2))

			worker.Stop()
			var err error
			Eventually(done, time.Minute).Should(Receive(&err))
			Expect(err).NotTo(HaveOccurred(), "being fenced out is not the worker's failure")
			Expect(f.runOfRef(ctx, taken).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING),
				"the run is left as it stands, for whoever holds the node now")
		})
	})

	Describe("Recovery at start", func() {
		// Scenario: A worker seals an orphaned attempt before its first claim
		//   Given a closed attempt's live activity file still on disk
		//   When the worker starts
		//   Then the file is a frame by the time reconciliation runs, before any claim
		It("seals an orphaned live file before reconciliation, and before the first claim", func() {
			By("given a closed attempt's live activity file still on disk")
			dir := GinkgoT().TempDir()
			created, err := f.store.CreateRun(ctx, connect.NewRequest(&millv1.CreateRunRequest{
				LeaseId:   "orphan-lease",
				NodeId:    "orphan-node",
				NodeRef:   "orphan#1",
				ContextId: "orphan-ctx",
				WorkerRef: f.worker,
				Branch:    "mill/orphan",
			}))
			Expect(err).NotTo(HaveOccurred())
			runID := created.Msg.GetRun().GetId()
			started, err := f.store.StartStepRun(ctx, connect.NewRequest(&millv1.StartStepRunRequest{
				LeaseId:   "orphan-lease",
				RunId:     runID,
				StepName:  "implement",
				Kind:      millv1.StepKind_STEP_KIND_SKILL,
				WorkerRef: f.worker,
			}))
			Expect(err).NotTo(HaveOccurred())
			live := run.ActivityPath(dir, runID, "implement", started.Msg.GetStepRun().GetAttempt())
			testsupport.AppendActivity(live, testsupport.TextActivity("hello"))
			_, err = f.store.CloseStepRun(ctx, connect.NewRequest(&millv1.CloseStepRunRequest{
				LeaseId:   "orphan-lease",
				StepRunId: started.Msg.GetStepRun().GetId(),
				Evidence: &millv1.Evidence{
					Kind:  millv1.EvidenceKind_EVIDENCE_KIND_COMMIT,
					Value: &millv1.Evidence_Commit{Commit: "0123456789abcdef0123456789abcdef01234567"},
				},
			}))
			Expect(err).NotTo(HaveOccurred())

			By("when the worker starts")
			client, counter := f.counted()
			sealed := func() bool {
				_, err := os.Stat(live)
				return errors.Is(err, os.ErrNotExist)
			}
			startup := &recordingStartup{claim: counter.claims.Load, also: func() string {
				return fmt.Sprintf("sealed: %v", sealed())
			}}
			worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Dagger = client; o.LogDir = dir }),
				func(o *run.WorkerOptions) { o.Startup = startup })
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Stop()
				Eventually(done, time.Minute).Should(Receive())
			})

			By("then the file is already a frame when reconciliation and eviction run, with no claim yet")
			Eventually(startup.seen, 30*time.Second, 200*time.Millisecond).Should(HaveLen(2))
			Expect(startup.seen()).To(Equal([]string{
				"reconcile (claims so far: 0) sealed: true",
				"evict (claims so far: 0) sealed: true",
			}))
			Expect(run.FramePath(dir, runID, "implement", started.Msg.GetStepRun().GetAttempt())).To(BeAnExistingFile())
		})
	})
})

// stubbornHarness is a session that does not go when it is asked. Its turn
// never ends on its own, and its Cancel — like the real adapter's, which
// returns only once the session's process group is gone — does not return
// until the process it is in is killed. It stands for a sidecar that
// outlives SIGTERM, which is the case a second stop exists for.
type stubbornHarness struct {
	started   chan struct{}
	killed    chan struct{}
	begun     sync.Once
	ended     sync.Once
	cancelled atomic.Bool
}

func newStubborn() *stubbornHarness {
	return &stubbornHarness{started: make(chan struct{}), killed: make(chan struct{})}
}

func (h *stubbornHarness) Start(_ context.Context, _ harness.Session, _ harness.Prompt, _ []harness.Attachment) (*harness.Turn, error) {
	turn := harness.NewTurn()
	go func() {
		h.begun.Do(func() { close(h.started) })
		<-h.killed
		turn.Finish(fake.NeverRan(harness.ReasonCancelled, "the session was killed"), nil)
	}()
	return turn, nil
}

func (h *stubbornHarness) Resume(ctx context.Context, session harness.Session, prompt harness.Prompt, attachments []harness.Attachment) (*harness.Turn, error) {
	return h.Start(ctx, session, prompt, attachments)
}

// Cancel waits for the session to go, and it never does on its own.
func (h *stubbornHarness) Cancel(ctx context.Context, _ harness.Session) error {
	h.cancelled.Store(true)
	<-ctx.Done()
	h.ended.Do(func() { close(h.killed) })
	return nil
}

var _ harness.Harness = (*stubbornHarness)(nil)
