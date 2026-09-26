package run_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1 "github.com/steading-ai/mill/gen/harness/v1"
	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// UpstreamWait, UpstreamStreak and UpstreamGiveUp are mill#57's whole policy
// for an outage, specified directly against their own inputs: an
// InspectResponse built by hand for UpstreamStreak, since that is the one
// fact a fresh worker or `mill explain` has to recover the streak from
// (Invariant 10), and plain durations for the other two.

var base = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func inspectWith(entries ...*millv1.StepRun) *millv1.InspectResponse {
	steps := make([]*millv1.StepRunDetail, 0, len(entries))
	for _, sr := range entries {
		steps = append(steps, &millv1.StepRunDetail{StepRun: sr})
	}
	return &millv1.InspectResponse{Steps: steps}
}

func refusedAttempt(step string, attempt int64, at time.Time) *millv1.StepRun {
	return &millv1.StepRun{
		StepName: step, Attempt: attempt, EndedAt: timestamppb.New(at),
		Exit: &harnessv1.ProcessExit{Reason: harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED},
	}
}

func closedAttempt(step string, attempt int64, at time.Time) *millv1.StepRun {
	return &millv1.StepRun{StepName: step, Attempt: attempt, EndedAt: timestamppb.New(at)}
}

var _ = Describe("UpstreamWait", Label("unit"), func() {
	It("doubles per consecutive refusal, capped at five minutes", func() {
		Expect(run.UpstreamWait(1)).To(Equal(15 * time.Second))
		Expect(run.UpstreamWait(2)).To(Equal(30 * time.Second))
		Expect(run.UpstreamWait(3)).To(Equal(time.Minute))
		Expect(run.UpstreamWait(4)).To(Equal(2 * time.Minute))
		Expect(run.UpstreamWait(5)).To(Equal(4 * time.Minute))
		Expect(run.UpstreamWait(6)).To(Equal(5 * time.Minute))
		Expect(run.UpstreamWait(20)).To(Equal(5 * time.Minute))
	})
})

var _ = Describe("UpstreamStreak", Label("unit"), func() {
	It("is zero when the step's latest attempt was not a refusal", func() {
		detail := inspectWith(
			refusedAttempt("gather-context", 1, base),
			closedAttempt("gather-context", 2, base.Add(time.Minute)),
		)
		count, since, reset := run.UpstreamStreak(detail, "gather-context")
		Expect(count).To(BeZero())
		Expect(since.IsZero()).To(BeTrue())
		Expect(reset.IsZero()).To(BeTrue())
	})

	It("counts the trailing run of refusals and when the first of them ended", func() {
		detail := inspectWith(
			closedAttempt("gather-context", 1, base),
			refusedAttempt("gather-context", 2, base.Add(5*time.Minute)),
			refusedAttempt("gather-context", 3, base.Add(7*time.Minute)),
		)
		count, since, _ := run.UpstreamStreak(detail, "gather-context")
		Expect(count).To(Equal(int64(2)))
		Expect(since).To(BeTemporally("==", base.Add(5*time.Minute)))
	})

	It("only counts the named step's attempts", func() {
		detail := inspectWith(
			refusedAttempt("gather-context", 1, base),
			refusedAttempt("implement", 1, base),
		)
		count, since, _ := run.UpstreamStreak(detail, "implement")
		Expect(count).To(Equal(int64(1)))
		Expect(since).To(BeTemporally("==", base))
	})

	It("takes the reset from the freshest attempt that named one", func() {
		older := refusedAttempt("gather-context", 1, base)
		older.Exit.ResetAt = timestamppb.New(base.Add(time.Hour))
		newer := refusedAttempt("gather-context", 2, base.Add(time.Minute))
		newer.Exit.ResetAt = timestamppb.New(base.Add(2 * time.Hour))
		detail := inspectWith(older, newer)
		count, _, reset := run.UpstreamStreak(detail, "gather-context")
		Expect(count).To(Equal(int64(2)))
		Expect(reset).To(BeTemporally("==", base.Add(2*time.Hour)))
	})

	It("falls back to an older attempt's reset when the freshest named none", func() {
		older := refusedAttempt("gather-context", 1, base)
		older.Exit.ResetAt = timestamppb.New(base.Add(time.Hour))
		newer := refusedAttempt("gather-context", 2, base.Add(time.Minute))
		detail := inspectWith(older, newer)
		_, _, reset := run.UpstreamStreak(detail, "gather-context")
		Expect(reset).To(BeTemporally("==", base.Add(time.Hour)))
	})
})

var _ = Describe("UpstreamGiveUp", Label("unit"), func() {
	It("is false with no streak", func() {
		Expect(run.UpstreamGiveUp(time.Time{}, time.Now())).To(BeFalse())
	})

	It("is false short of thirty minutes and true at or past it", func() {
		Expect(run.UpstreamGiveUp(base, base.Add(29*time.Minute))).To(BeFalse())
		Expect(run.UpstreamGiveUp(base, base.Add(30*time.Minute))).To(BeTrue())
		Expect(run.UpstreamGiveUp(base, base.Add(45*time.Minute))).To(BeTrue())
	})
})

// The end-to-end path: a step whose upstream refuses it a few times waits
// and asks again on its own, spending none of the step's retries, and an
// outage that outlasts the wait parks once with the outage named as the
// reason, not the same round reported over and over (mill#57's scenario).

var _ = Describe("A step the upstream refuses", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("waits out a short outage for free and closes on its own evidence", func() {
		By("given a gather-context step with retries 1 whose upstream refuses it twice")
		workflow := f.variant(ctx, "upstream-outage", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 1
		})
		var mu sync.Mutex
		var waits []time.Duration
		f.script(append([]fake.Script{refused("refused 1"), refused("refused 2")}, f.quickFixScripts()...)...)

		By("when the run is executed")
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.Sleep = func(_ context.Context, d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				waits = append(waits, d)
			}
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the refusals waited an escalating delay and cost the step nothing")
		mu.Lock()
		Expect(waits).To(Equal([]time.Duration{15 * time.Second, 30 * time.Second}))
		mu.Unlock()

		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(3))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[0].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(attempts[1].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		Expect(attempts[2].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(f.runOf(ctx).GetStatus()).NotTo(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		Expect(closedSteps(detail)).To(HaveKey("gather-context"))
	})

	It("parks once on the outage when it outlasts the wait, not the same round three times", func() {
		By("given a gather-context step whose upstream refuses it for over thirty minutes")
		workflow := f.variant(ctx, "upstream-giveup", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 5
		})

		var mu sync.Mutex
		clock := time.Now()
		advance := func(d time.Duration) func(harness.Session) {
			return func(harness.Session) {
				mu.Lock()
				defer mu.Unlock()
				clock = clock.Add(d)
			}
		}
		now := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		}
		refusedTurn := func(final string, do func(harness.Session)) fake.Script {
			return fake.Script{
				Terminal: &harness.Terminal{
					FinalMessage: final,
					Exit:         harness.ProcessExit{Status: 1, Reason: harness.ReasonUpstream},
					Usage:        usage(1),
				},
				Do: do,
			}
		}
		f.script(
			refusedTurn("refused 1", advance(10*time.Minute)),
			refusedTurn("refused 2", advance(10*time.Minute)),
			refusedTurn("refused 3", advance(15*time.Minute)),
		)

		By("when the run is executed")
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.Now = now
			o.Sleep = func(context.Context, time.Duration) {}
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then every refusal is on the ledger and the run parks, retries untouched")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(3))
		for _, a := range attempts {
			Expect(a.GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
			Expect(a.GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		}
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))

		By("and the human sees the outage once, not the same round repeated")
		comments := f.comments(ctx)
		Expect(comments).To(HaveLen(1), "one comment, not one per attempt")
		Expect(comments[0]).To(ContainSubstring("failed 1 times"), "one round, not three")
		Expect(comments[0]).To(ContainSubstring("outlasted the wait"))
		Expect(comments[0]).NotTo(ContainSubstring("Round 2"))
	})

	// mill#63: a reset the gateway itself observed is waited for, not
	// escalated past — even past what mill#57's own doubling would already
	// have moved on from, and never given up on merely because it is a long
	// wait. This drives a real request through a real gateway, since the
	// reset only ever reaches the ledger by way of Runner.gatewayUpstreamRefusal
	// reading what the gateway itself saw (internal/run/walk.go), never from
	// the fake harness's own exit classification.
	It("waits for a reset the upstream itself named instead of escalating", func() {
		By("given gather-context's one request is refused with a reset two minutes out")
		var refusals atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if refusals.Add(1) == 1 {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":%q,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", turnModel)
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}))
		DeferCleanup(upstream.Close)
		server := newTestGateway(upstream.URL, "sk-ant-oat01-upstream-reset-subscription")
		DeferCleanup(server.Close)

		request := func(session harness.Session) {
			req, err := http.NewRequest(http.MethodPost, session.Workspace.Env["ANTHROPIC_BASE_URL"]+"/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"ok"}]}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+session.Workspace.Env["ANTHROPIC_AUTH_TOKEN"])
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		workflow := f.variant(ctx, "upstream-reset", func(definition *millv1.Workflow) {
			stepOf(definition, "gather-context").Retries = 1
		})
		f.script(append([]fake.Script{
			{Terminal: &harness.Terminal{FinalMessage: "refused", Exit: harness.ProcessExit{Status: 1, Reason: harness.ReasonUpstream}, Usage: usage(1)}, Do: request},
		}, f.quickFixScripts()...)...)

		By("when the run is executed")
		var mu sync.Mutex
		var waits []time.Duration
		Expect(f.runner(func(o *run.Options) {
			o.Workflow = workflow
			o.Gateway = server.Gateway()
			o.Egress = workspace.Egress{Gateway: server.URL()}
			o.Sleep = func(_ context.Context, d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				waits = append(waits, d)
			}
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then it waited once for the reset, not the escalating sequence")
		mu.Lock()
		Expect(waits).To(HaveLen(1))
		Expect(waits[0]).To(BeNumerically("~", 120*time.Second+run.UpstreamResetMargin, 5*time.Second))
		mu.Unlock()

		By("and it closed rather than parking, with the reset recorded on the refused attempt")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(2))
		Expect(attempts[0].GetExit().GetReason()).To(Equal(harnessv1.ExitReason_EXIT_REASON_UPSTREAM_REFUSED))
		Expect(attempts[0].GetExit().GetResetAt()).NotTo(BeNil())
		Expect(attempts[1].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_CLOSED))
		Expect(f.runOf(ctx).GetStatus()).NotTo(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
	})
})

// newTestGateway starts a real gateway in front of upstream, the setup a
// gateway-observed reset (this file) and the worker-wide breaker
// (worker_test.go) each need a real *gateway.Gateway for, not a fake with a
// scripted Outage().
func newTestGateway(upstream, credential string) *gateway.Server {
	gw, err := gateway.New(gateway.Config{
		Upstream:   upstream,
		Credential: gateway.CredentialOf(credential),
		Log:        slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
	})
	Expect(err).NotTo(HaveOccurred())
	server, err := gateway.Listen("127.0.0.1:0", gw)
	Expect(err).NotTo(HaveOccurred())
	return server
}
