package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/skift"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
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
			node := f.createNode(ctx, &skiftv1.CreateNodeOp{
				Title: "the work behind the decision", Body: "Change the README's first line.",
				BlockedBy: []string{blocker},
			})
			f.script(f.quickFixScripts()...)

			client, counter := f.counted()
			worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client }),
				func(o *run.WorkerOptions) { o.Wait = 30 * time.Second })
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Drain()
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
			Expect(counter.events.Load()).To(BeZero(), "nothing subscribed to skift's change stream")
			Expect(counter.claims.Load()).To(BeNumerically("<=", 2),
				"one blocking claim, not a poll of its own")
		})

		It("blocks with no ready work and returns within a second of a node becoming ready", func() {
			blocker := f.userNode(ctx, "a decision only a person can make", "Say yes or no.")
			node := f.createNode(ctx, &skiftv1.CreateNodeOp{
				Title: "the work behind the decision", Body: "Change the README's first line.",
				BlockedBy: []string{blocker},
			})

			claimed := make(chan *skift.Claimed, 1)
			go func() {
				defer GinkgoRecover()
				got, err := f.client.ClaimNext(ctx, nil, nil, 30*time.Second)
				Expect(err).NotTo(HaveOccurred())
				claimed <- got
			}()
			Consistently(claimed, 2*time.Second, 250*time.Millisecond).ShouldNot(Receive(),
				"the claim blocks rather than returning empty")

			ready := time.Now()
			f.complete(ctx, blocker)

			// Skift polls its own ready set at most a second apart inside
			// the wait, so the bound is that second plus the round trip.
			var got *skift.Claimed
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
			worker.Drain()
			Eventually(done, 10*time.Second).Should(Receive(BeNil()))
		})

		It("claims nothing outside its project and label filters", func() {
			mine := f.labelledNode(ctx, "work this mill takes", "Change the README's first line.", "mill")
			theirs := f.labelledNode(ctx, "work for somebody else", "Not this mill's.")
			f.script(f.quickFixScripts()...)

			worker := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Labels = []string{"mill"} })
			done := startWorker(ctx, worker)
			Eventually(func() skiftv1.Status { return f.statusOf(ctx, mine) }, 2*time.Minute, 200*time.Millisecond).
				Should(Equal(skiftv1.Status_STATUS_DONE))
			worker.Drain()
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			Expect(f.statusOf(ctx, theirs)).To(Equal(skiftv1.Status_STATUS_OPEN))
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

			Eventually(func() []skiftv1.Status {
				return []skiftv1.Status{f.statusOf(ctx, first), f.statusOf(ctx, second)}
			}, 3*time.Minute, 200*time.Millisecond).Should(Equal([]skiftv1.Status{
				skiftv1.Status_STATUS_DONE, skiftv1.Status_STATUS_DONE,
			}))
			Consistently(done, time.Second).ShouldNot(Receive(), "the loop asks for more work rather than ending")

			worker.Drain()
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

	// mill#63: the worker-wide breaker. This drives a real *gateway.Gateway,
	// not a fake with a scripted Outage(): the bug step 2 fixed was in
	// exactly which condition clears g.outage, and a fake's Outage() could
	// only ever return what the spec told it to, never reproduce that.
	Describe("An upstream outage", func() {
		It("stops claiming until a probe shows the upstream answering again", func() {
			By("given a gateway whose upstream refuses every request, including the doctor's probe")
			var healthy atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !healthy.Load() {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/messages/count_tokens" {
					_, _ = w.Write([]byte(`{"input_tokens":1}`))
					return
				}
				_, _ = w.Write([]byte(`{"model":"claude-sonnet-4-5","id":"msg","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			DeferCleanup(upstream.Close)
			server := newTestGateway(upstream.URL, "sk-ant-oat01-worker-breaker-subscription")
			DeferCleanup(server.Close)
			gw := server.Gateway()

			By("and one request has already tripped the process-wide outage")
			gw.Rotate("outage-probe", gateway.Step{
				StepRunID: "outage-probe", StepName: "probe", Token: workspace.TokenFor("outage-probe-token"),
			})
			req, err := http.NewRequest(http.MethodPost, server.URL()+"/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"ok"}]}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer outage-probe-token")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			gw.Revoke("outage-probe")
			Expect(gw.Outage()).NotTo(BeNil())

			By("and a node ready for a worker wired to this gateway")
			node := f.node(ctx, "a fix behind the breaker", "Change the README's first line.")
			f.script(f.quickFixScripts()...)

			client, counter := f.counted()
			worker := f.workerOver(f.runner(quick, func(o *run.Options) {
				o.Skift = client
				o.Gateway = gw
			}), func(o *run.WorkerOptions) { o.Wait = time.Second })
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Drain()
				Eventually(done, time.Minute).Should(Receive())
			})

			By("then the worker reports waiting on upstream and claims nothing")
			Eventually(func() millv1.WorkerState { return worker.State() }, 2*time.Second, 20*time.Millisecond).
				Should(Equal(millv1.WorkerState_WORKER_STATE_WAITING_ON_UPSTREAM))
			Consistently(func() int64 { return counter.claims.Load() }, time.Second, 100*time.Millisecond).
				Should(BeZero(), "no claim is made while the outage stands")
			Expect(worker.Outage()).NotTo(BeNil())

			By("when the upstream starts answering again")
			healthy.Store(true)

			By("then the worker's next probe clears the outage and it claims and finishes the node")
			Eventually(func() skiftv1.Status { return f.statusOf(ctx, node) }, 2*time.Minute, 100*time.Millisecond).
				Should(Equal(skiftv1.Status_STATUS_DONE))
			Expect(worker.Outage()).To(BeNil())
			Expect(worker.State()).To(Equal(millv1.WorkerState_WORKER_STATE_CLAIMING))
		})
	})

	Describe("Stopping", func() {
		It("drains at the next step boundary, aborting nothing that already ran", func() {
			By("given work is executing the implement step of node N")
			node := f.node(ctx, "a fix drained mid-step", "Change the README's first line.")
			var worker *run.Worker
			scripts := f.quickFixScripts()
			implement := scripts[1]
			scripts[1] = fake.Script{Terminal: implement.Terminal, Do: func(session harness.Session) {
				// Draining mid-step must not touch it: walk() only checks
				// for a drain at the top of its loop, before an attempt
				// starts, never once one is already running.
				implement.Do(session)
				worker.Drain()
			}}
			f.script(scripts...)
			worker = f.workerOver(f.runner(quick))
			done := startWorker(ctx, worker)

			By("then the run finishes implement, releases N preempted/drain at the next boundary, and stops short of commit")
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			reason, detail := f.releasedLease(ctx)
			Expect(reason).To(Equal("preempted"))
			Expect(detail).To(Equal("drain"))
			Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the node is claimable again")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))

			detailAfter := f.inspect(ctx)
			implemented := stepRuns(detailAfter, "implement")
			Expect(implemented).To(HaveLen(1))
			Expect(implemented[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED),
				"a step already attempted finishes exactly as if no drain had been requested")
			Expect(stepRuns(detailAfter, "commit")).To(BeEmpty(),
				"a step never attempted is the one a drain releases in front of")

			By("and the next work claims N and resumes at commit")
			f.script(f.quickFixScripts()[2:]...)
			next := f.workerOver(f.runner(quick))
			resumed := startWorker(ctx, next)
			Eventually(func() skiftv1.Status { return f.statusOf(ctx, node) }, 3*time.Minute, 200*time.Millisecond).
				Should(Equal(skiftv1.Status_STATUS_DONE))
			next.Drain()
			Eventually(resumed, time.Minute).Should(Receive(BeNil()))
		})

		It("a drain releases at the boundary", func() {
			By("given two nodes held under this worker at concurrency two")
			first := f.node(ctx, "the first fix mid-drain", "Change the README's first line.")
			second := f.node(ctx, "the second fix mid-drain", "Change the README's second line.")
			var worker *run.Worker
			implementDone := fake.Script{
				Terminal: fake.Terminal("the change is made", nil, usage(20)),
				Do:       func(harness.Session) { worker.Drain() },
			}
			f.script(ok("context gathered"), ok("context gathered"), implementDone, implementDone)
			client, counter := f.counted()
			worker = f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client }),
				func(o *run.WorkerOptions) { o.Concurrency = 2 })
			done := startWorker(ctx, worker)
			Eventually(func() *millv1.Run { return f.runOfRef(ctx, first) }, time.Minute, 100*time.Millisecond).
				ShouldNot(BeNil())
			Eventually(func() *millv1.Run { return f.runOfRef(ctx, second) }, time.Minute, 100*time.Millisecond).
				ShouldNot(BeNil())

			By("and a third, unrelated node ready with no free slot at that concurrency")
			third := f.createNode(ctx, &skiftv1.CreateNodeOp{
				Title: "a third fix with no free slot", Body: "Change the README's third line.",
			})

			By("then both runs reach their next boundary, and the worker exits zero with neither aborted")
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			for _, ref := range []string{first, second} {
				released := f.runOfRef(ctx, ref)
				Expect(released.GetLeaseId()).To(BeEmpty(), ref+"'s lease was released, not held past the boundary")
				Expect(released.GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))
				detail := f.inspectRun(ctx, released.GetId())
				for _, name := range []string{"gather-context", "implement", "commit", "clean-tree", "pr", "merge"} {
					for _, sr := range stepRuns(detail, name) {
						Expect(sr.GetStatus()).NotTo(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED),
							"no attempt is aborted by a drain, whatever boundary it reached")
					}
				}
			}

			By("and neither release's freed slot was used to claim the third node")
			Expect(f.statusOf(ctx, third)).To(Equal(skiftv1.Status_STATUS_OPEN))
			Expect(f.runOfRef(ctx, third)).To(BeNil())
			Expect(counter.claims.Load()).To(Equal(int64(2)),
				"draining stops claiming; a slot a release frees claims nothing more")
		})

		It("drains in front of a step's own retry, not only a step never attempted", func() {
			By("given gather-context fails once, requesting a drain during the attempt that earns it a retry")
			node := f.node(ctx, "a fix drained mid-retry", "Change the README's first line.")
			var worker *run.Worker
			round1 := failed("round 1")
			scripts := append([]fake.Script{{Terminal: round1.Terminal, Do: func(session harness.Session) {
				// The failed attempt has already closed by the time walk()'s
				// loop reaches the top again for gather-context's retry:
				// nothing is in flight there, so the drain releases in
				// front of it exactly as it would in front of any other
				// step's next attempt.
				worker.Drain()
			}}}, f.quickFixScripts()...)
			f.script(scripts...)
			worker = f.workerOver(f.runner(quick))
			done := startWorker(ctx, worker)

			By("then the run releases in front of gather-context's retry, which never starts")
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			reason, detail := f.releasedLease(ctx)
			Expect(reason).To(Equal("preempted"))
			Expect(detail).To(Equal("drain"))
			Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the node is claimable again")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))

			detailAfter := f.inspect(ctx)
			gathered := stepRuns(detailAfter, "gather-context")
			Expect(gathered).To(HaveLen(1), "the failed attempt only; its retry is the one a drain releases in front of")
			Expect(gathered[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))

			By("and the next worker resumes it as a fresh attempt of the same step")
			f.script(f.quickFixScripts()...)
			next := f.workerOver(f.runner(quick))
			resumed := startWorker(ctx, next)
			Eventually(func() skiftv1.Status { return f.statusOf(ctx, node) }, 3*time.Minute, 200*time.Millisecond).
				Should(Equal(skiftv1.Status_STATUS_DONE))
			next.Drain()
			Eventually(resumed, time.Minute).Should(Receive(BeNil()))
		})

		It("drains in front of a review that would answer a revise", func() {
			By("given a feature run whose review attempt 1 asked for changes and whose implement round 2 has just closed through pr")
			f.pushWorkflowFrom(ctx, featureDir)
			node := f.node(ctx, "a fix drained before a second review", "Change the README's first line.")
			var worker *run.Worker
			writesPlan := func(harness.Session) {
				defer GinkgoRecover()
				Expect(f.workspace.Write(context.Background(), planPath(f.nodeRef),
					[]byte(planBody(f.runOf(context.Background()).GetId(), f.nodeRef)))).To(Succeed())
			}
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
				fake.Script{Terminal: fake.Terminal("the change is made again", nil, usage(20)), Do: f.commits},
				fake.Script{Terminal: fake.Terminal("simplified again", nil, usage(10)), Do: f.commits},
				fake.Script{Terminal: fake.Terminal("committed again", nil, usage(5)), Do: f.commits},
				fake.Script{Terminal: fake.Terminal("opened the pull request again", nil, usage(5)), Do: func(session harness.Session) {
					f.commits(session)
					// review already has a StepRun row from its first
					// attempt — exactly the boundary the old stepAttempted
					// check would have skipped.
					worker.Drain()
				}},
			)
			worker = f.workerOver(f.runner(quick, func(o *run.Options) {
				o.Workflow = "feature"
				o.WorkflowDir = mustAbs(featureDir)
			}))
			done := startWorker(ctx, worker)

			By("then the run releases in front of review's second attempt, which never starts")
			Eventually(done, time.Minute).Should(Receive(BeNil()))

			reason, detail := f.releasedLease(ctx)
			Expect(reason).To(Equal("preempted"))
			Expect(detail).To(Equal("drain"))
			Expect(f.leaseOf(ctx).GetId()).To(BeEmpty(), "the node is claimable again")
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))

			detailAfter := f.inspect(ctx)
			prs := stepRuns(detailAfter, "pr")
			Expect(prs).To(HaveLen(2), "both pr attempts ran, uninterrupted: the drain caught the boundary after")
			for _, name := range []string{"implement", "simplify", "commit", "clean-tree", "pr"} {
				for _, sr := range stepRuns(detailAfter, name) {
					Expect(sr.GetStatus()).NotTo(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_ABORTED),
						"no attempt is aborted by a drain")
				}
			}
			reviews := stepRuns(detailAfter, "review")
			Expect(reviews).To(HaveLen(1), "review's first attempt only; its second is the one a drain releases in front of")
			Expect(reviews[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_SUPERSEDED))

			By("and the next worker resumes at review as a fresh attempt")
			f.script(fake.Script{Terminal: fake.Terminal("", approves("AC-2", "the fix addresses the finding"), usage(5))})
			next := f.workerOver(f.runner(quick, func(o *run.Options) {
				o.Workflow = "feature"
				o.WorkflowDir = mustAbs(featureDir)
			}))
			resumed := startWorker(ctx, next)
			Eventually(func() skiftv1.Status { return f.statusOf(ctx, node) }, 3*time.Minute, 200*time.Millisecond).
				Should(Equal(skiftv1.Status_STATUS_DONE))
			next.Drain()
			Eventually(resumed, time.Minute).Should(Receive(BeNil()))
		})

		It("lets the run finish normally when the drain lands on its last step, with nothing left to run", func() {
			By("given a workflow with nothing after pr, the run's last step")
			workflow := f.variant(ctx, "quick-fix-no-merge", func(definition *millv1.Workflow) {
				steps := make([]*millv1.Step, 0, len(definition.GetSteps()))
				for _, step := range definition.GetSteps() {
					if step.GetName() == "merge" {
						continue
					}
					steps = append(steps, step)
				}
				definition.Steps = steps
			})
			node := f.node(ctx, "a fix drained on its last step", "Change the README's first line.")
			var worker *run.Worker
			scripts := f.quickFixScripts()
			pr := scripts[3]
			scripts[3] = fake.Script{Terminal: pr.Terminal, Do: func(session harness.Session) {
				pr.Do(session)
				worker.Drain()
			}}
			f.script(scripts...)
			worker = f.workerOver(f.runner(quick, func(o *run.Options) { o.Workflow = workflow }))
			done := startWorker(ctx, worker)

			By("then the run finishes on its own: there is no boundary left for the drain to land on")
			Eventually(done, time.Minute).Should(Receive(BeNil()))
			Expect(f.statusOf(ctx, node)).To(Equal(skiftv1.Status_STATUS_DONE))
			Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_DONE))

			reason, _ := f.releasedLease(ctx)
			Expect(reason).NotTo(Equal("preempted"), "the run finished on its own, not by a drain")
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
			worker.Drain()
			Consistently(done, 2*time.Second).ShouldNot(Receive())

			By("the second kills it")
			kill()
			var err error
			Eventually(done, time.Minute).Should(Receive(&err))
			Expect(err).To(MatchError(run.ErrKilled))
			Expect(stubborn.cancelled.Load()).To(BeTrue(), "the session was cancelled, not left behind")
		})
	})

	// mill#93: a drain's own in-flight set is this worker's inFlight map
	// alone (Worker.spawn/InFlight), never the ledger, so a parked run (a
	// gate answered and waiting for a claim) or a cancelled one (an aborted
	// attempt, no worker driving it) can neither enter it nor hold it open.
	Describe("Draining alongside parked and cancelled runs it cannot claim", func() {
		It("reaches idle at the live step's own boundary, not a timeout the other runs never affect", func() {
			node := f.node(ctx, "a fix drained beside runs nobody holds", "Change the README's first line.")
			f.parkedRun(ctx, project+"#910")
			f.parkedRun(ctx, project+"#911")
			f.abortedRun(ctx, project+"#912", "somebody-else@host")

			var worker *run.Worker
			implementDone := fake.Script{
				Terminal: fake.Terminal("the change is made", nil, usage(20)),
				Do:       func(harness.Session) { worker.Drain() },
			}
			f.script(ok("context gathered"), implementDone)
			worker = f.workerOver(f.runner(quick))
			done := startWorker(ctx, worker)

			Eventually(func() millv1.WorkerState { return worker.State() }, 5*time.Second, 20*time.Millisecond).
				Should(Equal(millv1.WorkerState_WORKER_STATE_IDLE),
					"the live step's own boundary is what clears inFlight, well inside a drain timeout")
			Eventually(done, time.Minute).Should(Receive(BeNil()))
			Expect(f.runOfRef(ctx, node).GetLeaseId()).To(BeEmpty(), "the live run released, not aborted")
		})

		It("reports idle at once when nothing is held, parked and cancelled runs notwithstanding", func() {
			f.parkedRun(ctx, project+"#920")
			f.parkedRun(ctx, project+"#921")
			f.abortedRun(ctx, project+"#922", "somebody-else@host")

			worker := f.workerOver(f.runner(quick))
			worker.Drain()
			Expect(worker.State()).To(Equal(millv1.WorkerState_WORKER_STATE_IDLE),
				"nothing was ever claimed, so nothing in the ledger is this worker's to wait on")
		})
	})

	Describe("Losing a run", func() {
		It("skips a node it was fenced out of and goes back to claiming", func() {
			taken := f.node(ctx, "a node taken by somebody else", "Change the README's first line.")
			// Mid-step, another principal takes the lease. The next
			// heartbeat is refused; the worker's own reclaim loses the race,
			// so it releases the run (stopped) and abandons it locally.
			f.script(fake.Script{Hold: true, Do: func(harness.Session) {
				defer GinkgoRecover()
				f.expire(ctx, f.leaseOf(ctx).GetId())
				f.reclaim(ctx)
			}})

			client, counter := f.counted()
			worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client }))
			done := startWorker(ctx, worker)

			// A second claim is the worker carrying on: it asked for more
			// work rather than ending on somebody else's node.
			Eventually(func() int64 { return counter.claims.Load() }, time.Minute, 100*time.Millisecond).
				Should(BeNumerically(">=", 2))

			worker.Drain()
			var err error
			Eventually(done, time.Minute).Should(Receive(&err))
			Expect(err).NotTo(HaveOccurred(), "being fenced out is not the worker's failure")
			Expect(f.runOfRef(ctx, taken).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED),
				"the failed reclaim releases the run, for whoever holds the node now")
		})
	})

	Describe("A claim it cannot open, for a reason it did not choose", func() {
		It("waits before reclaiming, with a bound that grows, and logs once per attempt rather than per cycle", func() {
			f.createNode(ctx, &skiftv1.CreateNodeOp{Title: "a workspace that never opens", Body: "body"})

			boom := errors.New("workspace: the sandbox host is unreachable")

			warnings := &warnCounter{message: "could not open a claimed node; backing off before reclaiming it"}
			client, counter := f.counted()
			worker := f.workerOver(f.runner(quick, func(o *run.Options) {
				o.Skift = client
				o.Workspaces = func(context.Context, workspace.Spec, run.Repository) (run.Workspace, error) {
					return nil, boom
				}
			}), func(o *run.WorkerOptions) { o.Log = slog.New(warnings) })
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Drain()
				Eventually(done, time.Minute).Should(Receive(BeNil()))
			})

			By("the node is opened once, and not reclaimed again inside its own backoff window")
			Eventually(warnings.Count, 2*time.Second, 20*time.Millisecond).Should(BeNumerically(">=", 1))
			Consistently(warnings.Count, 700*time.Millisecond, 50*time.Millisecond).Should(Equal(1),
				"BackoffBase is a second; a reclaim well inside it would be the old hot loop")

			By("then the window elapses and it is tried again, waiting longer each time")
			Eventually(warnings.Count, 4*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", 2))
			Eventually(warnings.Count, 8*time.Second, 100*time.Millisecond).Should(BeNumerically(">=", 3))

			attempts := warnings.Times()
			gap1 := attempts[1].Sub(attempts[0])
			gap2 := attempts[2].Sub(attempts[1])
			total := len(attempts)
			Expect(gap2).To(BeNumerically(">", gap1), "each refusal waits longer than the one before it")

			By("skift's ClaimNext is asked more often than the node is actually opened")
			Expect(counter.claims.Load()).To(BeNumerically(">", int64(total)),
				"at least one claim never reached a workspace: it backed off and was released at once")
		})
	})

	Describe("A project with no repository", func() {
		It("holds each node it claims there, and warns once rather than once per node", func() {
			first := f.createNode(ctx, &skiftv1.CreateNodeOp{Title: "first with no repository", Body: "body"})
			second := f.createNode(ctx, &skiftv1.CreateNodeOp{Title: "second with no repository", Body: "body"})

			// Repositories is resolved once per claim, from the project the
			// claim carried, before either node is known apart: the project
			// has only these two ready nodes and nothing else for skift to
			// hand back, so a call count that stops at two once both are held
			// is the same proof a per-node counter would be (mill#39's
			// Scenario: "the worker claims each once... and does not claim it
			// again within the TTL").
			var calls atomic.Int64
			warnings := &warnCounter{message: "a project has no repository; nodes there are held rather than claimed"}
			worker := f.workerOver(f.runner(quick, func(o *run.Options) {
				o.Repositories = func(context.Context, *skiftv1.Project) (run.Repository, error) {
					calls.Add(1)
					return run.Repository{}, &forge.NoVCSURLError{Project: project}
				}
			}), func(o *run.WorkerOptions) {
				o.Log = slog.New(warnings)
			})
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Drain()
				Eventually(done, time.Minute).Should(Receive(BeNil()))
			})

			Eventually(func() []string {
				return []string{f.holdReasonOf(ctx, first), f.holdReasonOf(ctx, second)}
			}, time.Minute, 100*time.Millisecond).Should(And(
				HaveEach(HavePrefix("no repository")),
			), "both claimed and held rather than reclaimed into the same refusal")

			Consistently(warnings.Count, time.Second, 100*time.Millisecond).Should(Equal(1),
				"the second node's failure is the same project already warned about")

			By("and neither node is reclaimed again within the lease TTL, the Scenario's own Then clause")
			Expect(calls.Load()).To(Equal(int64(2)), "one claim per node, no more, once both are held")
			Consistently(calls.Load, ttl+time.Second, 100*time.Millisecond).
				Should(Equal(int64(2)), "skift's lease table gains one row per node, not thousands")
		})
	})

	Describe("Resuming at start", func() {
		// Scenario (mill#44): a worker with concurrency 2 running steading#4
		// at implement and mill#33 at review, when it restarts, its first
		// two claims are steading#4 and mill#33, and no new node is claimed
		// until one of them frees a slot.
		It("the next worker takes drained runs first", func() {
			By("given two nodes held under this worker at concurrency two")
			first := f.node(ctx, "the first interrupted fix", "Change the README's first line.")
			second := f.node(ctx, "the second interrupted fix", "Change the README's second line.")
			var setup *run.Worker
			// implement is each node's second session; StartStepRun records the
			// attempt before this runs (internal/run/walk.go's step), so the
			// step it belongs to always finishes — only commit, never
			// attempted, is what draining gives back before starting.
			implementDone := fake.Script{
				Terminal: fake.Terminal("the change is made", nil, usage(20)),
				Do:       func(harness.Session) { setup.Drain() },
			}
			f.script(ok("context gathered"), ok("context gathered"), implementDone, implementDone)
			setup = f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Concurrency = 2 })
			setupDone := startWorker(ctx, setup)
			Eventually(setupDone, time.Minute).Should(Receive(BeNil()))
			firstRunID, secondRunID := f.runOfRef(ctx, first).GetId(), f.runOfRef(ctx, second).GetId()
			Expect(f.runOfRef(ctx, first).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))
			Expect(f.runOfRef(ctx, second).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_STOPPED))

			By("and a third, unrelated node ready but with no free slot at that concurrency")
			third := f.createNode(ctx, &skiftv1.CreateNodeOp{
				Title: "a third fix with no free slot", Body: "Change the README's third line.",
			})

			By("when the next worker starts at the same concurrency")
			journal := &bytes.Buffer{}
			client, counter := f.counted()
			// Both reclaimed runs pick up at commit, a real one for each; a
			// single held script for both, cleaned up by killing the worker
			// rather than draining it, keeps the two from committing to the
			// fixture's one shared workspace at once (support_test.go).
			f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})
			next := f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client }),
				func(o *run.WorkerOptions) { o.Concurrency = 2; o.Out = journal })
			killable, kill := context.WithCancel(ctx)
			resumed := startWorker(killable, next)
			DeferCleanup(func() {
				kill()
				Eventually(resumed, time.Minute).Should(Receive())
			})

			By("then its first two claims are the two runs it held, resumed rather than claimed fresh")
			Eventually(func() int64 { return counter.claims.Load() }, time.Minute, 100*time.Millisecond).
				Should(Equal(int64(2)))
			Expect(f.runOfRef(ctx, first).GetId()).To(Equal(firstRunID), "the same run, not a new claim")
			Expect(f.runOfRef(ctx, second).GetId()).To(Equal(secondRunID), "the same run, not a new claim")
			Expect(f.runOfRef(ctx, first).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
			Expect(f.runOfRef(ctx, second).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_RUNNING))
			Expect(journal.String()).To(ContainSubstring("resumed " + first + ", last updated"))
			Expect(journal.String()).To(ContainSubstring("resumed " + second + ", last updated"))

			By("and each resumes at its next step in a fresh session, not a resume of implement's")
			calls := f.harness.Calls()
			Expect(calls).To(HaveLen(2), "one fresh session per reclaimed run's commit step")
			for _, call := range calls {
				Expect(call.Verb).To(Equal("start"), "commit has never been attempted before; nothing exists to resume")
			}

			By("and no new node is claimed until one of them frees a slot")
			Consistently(func() skiftv1.Status { return f.statusOf(ctx, third) }, 2*time.Second, 100*time.Millisecond).
				Should(Equal(skiftv1.Status_STATUS_OPEN))
			Consistently(func() int64 { return counter.claims.Load() }, 2*time.Second, 100*time.Millisecond).
				Should(Equal(int64(2)), "the third slot never opens because nothing frees the two reclaim took")
		})

		It("still claims fresh work with whatever concurrency reclaim did not use", func() {
			By("given one node held under this worker")
			resumable := f.node(ctx, "the interrupted fix", "Change the README's first line.")
			var setup *run.Worker
			implementDone := fake.Script{
				Terminal: fake.Terminal("the change is made", nil, usage(20)),
				Do:       func(harness.Session) { setup.Drain() },
			}
			f.script(ok("context gathered"), implementDone)
			setup = f.workerOver(f.runner(quick))
			setupDone := startWorker(ctx, setup)
			Eventually(setupDone, time.Minute).Should(Receive(BeNil()))
			runID := f.runOfRef(ctx, resumable).GetId()

			By("and a second, unrelated node also ready")
			fresh := f.node(ctx, "an unrelated fix", "Change the README's second line.")
			// A run row exists as soon as the claim lands (openRun, before any
			// workspace or session work) — proving the claim needs no more
			// than that, so this holds rather than driving either run through
			// a real commit concurrently with the other's, which races the
			// fixture's single f.workspace field (support_test.go, shared
			// across whichever run last attached or created one, and read by
			// f.commits).
			f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil)})

			By("when the next worker starts with a spare slot")
			journal := &bytes.Buffer{}
			next := f.workerOver(f.runner(quick), func(o *run.WorkerOptions) { o.Concurrency = 2; o.Out = journal })
			killable, kill := context.WithCancel(ctx)
			resumed := startWorker(killable, next)
			DeferCleanup(func() {
				kill()
				Eventually(resumed, time.Minute).Should(Receive())
			})

			By("then the released run is resumed and the spare slot still claims the fresh node")
			Eventually(func() *millv1.Run { return f.runOfRef(ctx, fresh) }, time.Minute, 100*time.Millisecond).
				ShouldNot(BeNil(), "reclaim took only one slot; the other went to an ordinary claim")
			Expect(f.runOfRef(ctx, resumable).GetId()).To(Equal(runID), "the same run, not a new claim")
			Expect(journal.String()).To(ContainSubstring("resumed " + resumable + ", last updated"))
			Expect(journal.String()).NotTo(ContainSubstring("resumed " + fresh))
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
			worker := f.workerOver(f.runner(quick, func(o *run.Options) { o.Skift = client; o.LogDir = dir }),
				func(o *run.WorkerOptions) { o.Startup = startup })
			done := startWorker(ctx, worker)
			DeferCleanup(func() {
				worker.Drain()
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

// warnCounter counts the log records whose message is the one named, so a
// spec can assert a warning was collapsed to one line rather than parse log
// text for it. It also keeps each record's time, so a spec that needs to
// know when a warning fired does not need instrumentation of its own.
type warnCounter struct {
	message string

	mu    sync.Mutex
	count int
	times []time.Time
}

func (c *warnCounter) Enabled(context.Context, slog.Level) bool { return true }

func (c *warnCounter) Handle(_ context.Context, record slog.Record) error {
	if record.Message == c.message {
		c.mu.Lock()
		c.count++
		c.times = append(c.times, record.Time)
		c.mu.Unlock()
	}
	return nil
}

func (c *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *warnCounter) WithGroup(string) slog.Handler      { return c }

func (c *warnCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func (c *warnCounter) Times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.times...)
}

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
