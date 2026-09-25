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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/harness"
	"github.com/steading-ai/mill/internal/harness/fake"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/testsupport"
	"github.com/steading-ai/mill/internal/workspace"
)

// The credential gateway as the run loop drives it (docs/sandbox.md
// §Credentials): each attempt's token, budget and pin are installed in it
// beside the workspace's own rotation, the session is pointed at it by the
// workspace's egress, what it counts lands in the ledger as the gateway's
// own rows, and a budget it refused on is the attempt's failure.

// The response the stand-in upstream streams: twenty tokens a turn.
const (
	turnModel      = "claude-sonnet-4-5-20250929"
	turnInput      = 8
	turnOutput     = 4
	turnCacheRead  = 6
	turnCacheWrite = 2
	turnTokens     = turnInput + turnOutput + turnCacheRead + turnCacheWrite
)

// gatewayFixture is a gateway on a real listener in front of a stand-in
// upstream, wired into the runner the way `mill run --gateway` wires one.
type gatewayFixture struct {
	server   *gateway.Server
	upstream *httptest.Server

	mu       sync.Mutex
	statuses []int
	envs     []map[string]string
}

func startGateway() *gatewayFixture {
	GinkgoHelper()
	g := &gatewayFixture{}
	g.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":%q,\"usage\":{\"input_tokens\":%d,\"cache_creation_input_tokens\":%d,\"cache_read_input_tokens\":%d,\"output_tokens\":1}}}\n\n",
			turnModel, turnInput, turnCacheWrite, turnCacheRead)
		_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", turnOutput)
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	DeferCleanup(g.upstream.Close)
	handler, err := gateway.New(gateway.Config{
		Upstream:   g.upstream.URL,
		Credential: gateway.CredentialOf("sk-ant-oat01-run-spec-subscription"),
		Log:        slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
	})
	Expect(err).NotTo(HaveOccurred())
	g.server, err = gateway.Listen("127.0.0.1:0", handler)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(g.server.Close)
	return g
}

// options wires the gateway into a runner as the CLI does.
func (g *gatewayFixture) options(o *run.Options) {
	o.Gateway = g.server.Gateway()
	o.Egress = workspace.Egress{Gateway: g.server.URL()}
}

// requests is what a scripted session does with the gateway: it reads
// where to go and what to present off its environment, exactly as the CLI
// would, and makes n model requests. Every status is kept for the spec.
func (g *gatewayFixture) requests(n int) func(session harness.Session) {
	return func(session harness.Session) {
		defer GinkgoRecover()
		env := session.Workspace.Env
		g.mu.Lock()
		g.envs = append(g.envs, env)
		g.mu.Unlock()
		Expect(env).To(HaveKeyWithValue("ANTHROPIC_BASE_URL", g.server.URL()))
		Expect(env).To(HaveKeyWithValue("ANTHROPIC_AUTH_TOKEN", session.Workspace.Token))
		for range n {
			req, err := http.NewRequest(http.MethodPost, env["ANTHROPIC_BASE_URL"]+"/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"ok"}]}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+env["ANTHROPIC_AUTH_TOKEN"])
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			g.mu.Lock()
			g.statuses = append(g.statuses, resp.StatusCode)
			g.mu.Unlock()
		}
	}
}

func (g *gatewayFixture) seen() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int{}, g.statuses...)
}

var _ = Describe("The credential gateway in the run loop", Label(testsupport.DBLabel), func() {
	var f *fixture
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
		f = start(ctx)
		f.node(ctx, "one line change", "Change the README's first line.")
	})

	It("A step over its budget is refused", func() {
		By("given step 3 — implement — has a token budget of one turn, and consumes it")
		g := startGateway()
		workflow := f.variant(ctx, "budgeted", func(definition *millv1.Workflow) {
			implement := stepOf(definition, "implement")
			implement.TokenBudget = turnTokens
			// One attempt: the budget is the reason, and a retry would
			// only spend another budget the same way.
			implement.Retries = 0
			implement.OnFailure = nil
		})
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: f.commits},
			// The implementer makes one request, which the upstream
			// answers, and then another, which the gateway refuses; it
			// then says it is done and exits cleanly, as a session that
			// took a 429 in its stride would.
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: func(session harness.Session) {
				g.requests(2)(session)
				f.commits(session)
			}},
		)

		By("when the session makes another request")
		Expect(f.runner(g.options, func(o *run.Options) { o.Workflow = workflow }).
			Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the gateway refuses with 429 naming the budget")
		Expect(g.seen()).To(Equal([]int{http.StatusOK, http.StatusTooManyRequests}))

		By("and the step fails with the budget as evidence")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "implement")
		Expect(attempts).To(HaveLen(1))
		Expect(attempts[0].GetStatus()).To(Equal(millv1.StepRunStatus_STEP_RUN_STATUS_FAILED))
		Expect(f.runOf(ctx).GetStatus()).To(Equal(millv1.RunStatus_RUN_STATUS_PARKED))
		body := f.theGate(ctx).GetBody()
		Expect(body).To(ContainSubstring(fmt.Sprintf("token budget of %d tokens", turnTokens)))
		Expect(body).To(ContainSubstring("step implement has used"))

		By("and the ledger holds the gateway's own count for the attempt, beside the adapter's")
		var implement *millv1.StepRunDetail
		for _, d := range detail.GetSteps() {
			if d.GetStepRun().GetId() == attempts[0].GetId() {
				implement = d
			}
		}
		Expect(implement.GetGatewayTotals()).To(HaveLen(1))
		counted := implement.GetGatewayTotals()[0]
		Expect(counted.GetSource()).To(Equal(millv1.UsageSource_USAGE_SOURCE_GATEWAY))
		Expect(counted.GetModel()).To(Equal(turnModel))
		Expect(counted.GetInputTokens()).To(Equal(int64(turnInput)))
		Expect(counted.GetOutputTokens()).To(Equal(int64(turnOutput)))
		Expect(counted.GetCacheReadTokens()).To(Equal(int64(turnCacheRead)))
		Expect(counted.GetCacheWriteTokens()).To(Equal(int64(turnCacheWrite)))
		Expect(implement.GetTotals()).To(HaveLen(1), "the adapter's report is its own row")
		Expect(implement.GetTotals()[0].GetSource()).To(Equal(millv1.UsageSource_USAGE_SOURCE_SDK))
		Expect(implement.GetTotals()[0].GetInputTokens()).To(Equal(int64(20)))

		By("and skift was posted the adapter's totals, not the gateway's on top")
		Expect(f.skiftUsage(ctx)).To(HaveKeyWithValue("claude-sonnet", int64(30)))
		Expect(f.skiftUsage(ctx)).NotTo(HaveKey(turnModel))
	})

	// Scenario: The assessment reaches the model through the gateway
	//   Given a gateway configured and a project with no default workflow
	//   When the assessing session makes a model request with its token
	//   Then the gateway accepts it, and the assessment's ledger row
	//   carries what the gateway counted
	//
	// The assessment is step 0, run outside the walk; its token has to be
	// installed in the gateway like every other attempt's, or the first
	// session of every run on a machine is refused before it says a word.
	It("The assessment reaches the model through the gateway", func() {
		By("given a gateway, and no project default so the assessment runs")
		g := startGateway()
		f.pushWorkflowFrom(ctx, buildWorkflowDir)
		assess := assessed("quick-fix", "quick-fix", "README.md is the only file the ask names")
		assess.Do = g.requests(1)
		f.script(append([]fake.Script{assess}, f.quickFixScripts()...)...)

		By("when the run goes through")
		Expect(f.runner(g.options, func(o *run.Options) {
			o.Workflow = ""
			o.Tier = millv1.Tier_TIER_UNSPECIFIED
		}).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then the assessing session was accepted with its own token")
		Expect(g.seen()).To(Equal([]int{http.StatusOK}))
		Expect(g.envs).To(HaveLen(1))

		By("and the assessment's row in the ledger carries the gateway's count")
		detail := f.inspect(ctx)
		Expect(detail.GetSteps()[0].GetStepRun().GetStepName()).To(Equal("assess"))
		Expect(detail.GetSteps()[0].GetGatewayTotals()).To(HaveLen(1))
		counted := detail.GetSteps()[0].GetGatewayTotals()[0]
		Expect(counted.GetSource()).To(Equal(millv1.UsageSource_USAGE_SOURCE_GATEWAY))
		Expect(counted.GetModel()).To(Equal(turnModel))
		Expect(counted.GetInputTokens() + counted.GetOutputTokens() + counted.GetCacheReadTokens() + counted.GetCacheWriteTokens()).To(Equal(int64(turnTokens)))
	})

	It("The local backend passes the same two variables when configured, and none when not", func() {
		By("given a gateway configured on a local run")
		g := startGateway()
		f.script(
			fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: func(session harness.Session) {
				g.requests(1)(session)
				f.commits(session)
			}},
			fake.Script{Terminal: fake.Terminal("the change is made", nil, usage(20)), Do: func(session harness.Session) {
				g.requests(1)(session)
				f.commits(session)
			}},
			fake.Script{Terminal: fake.Terminal("committed", nil, usage(5)), Do: f.commits},
			fake.Script{Terminal: fake.Terminal("opened the pull request", nil, usage(5)), Do: f.commits},
		)

		By("when the run goes through")
		Expect(f.runner(g.options).Execute(ctx, f.nodeRef)).To(Succeed())

		By("then every session was pointed at the gateway with its own step's token, and each was accepted")
		Expect(g.seen()).To(Equal([]int{http.StatusOK, http.StatusOK}))
		Expect(g.envs).To(HaveLen(2))
		Expect(g.envs[0]["ANTHROPIC_AUTH_TOKEN"]).NotTo(Equal(g.envs[1]["ANTHROPIC_AUTH_TOKEN"]), "the token follows the step")
		Expect(g.envs[0]).To(HaveKeyWithValue("ENABLE_TOOL_SEARCH", "true"))

		By("and the ledger has a gateway row for each of those steps")
		detail := f.inspect(ctx)
		counted := 0
		for _, d := range detail.GetSteps() {
			counted += len(d.GetGatewayTotals())
		}
		Expect(counted).To(Equal(2))

		By("and a runner with no gateway hands its sessions neither variable")
		f2 := start(ctx)
		f2.node(ctx, "another change", "Change the README's second line.")
		var env map[string]string
		f2.script(fake.Script{Terminal: fake.Terminal("context gathered", nil, usage(10)), Do: func(session harness.Session) {
			env = session.Workspace.Env
			f2.commits(session)
		}})
		Expect(f2.runner().Execute(ctx, f2.nodeRef)).To(Succeed())
		Expect(env).NotTo(HaveKey("ANTHROPIC_BASE_URL"))
		Expect(env).NotTo(HaveKey("ANTHROPIC_AUTH_TOKEN"))
	})

	// Scenario: Usage recorded after a reclaim reaches the ledger
	//   Given a step in flight whose run's lease was reclaimed by the
	//     heartbeat
	//   When the gateway forwards the next response
	//   Then its usage is recorded in the ledger under the new lease and
	//     no fenced write is logged
	It("Usage recorded after a reclaim reaches the ledger under the new lease", func() {
		By("given a gateway, and a step held open across a reclaim")
		g := startGateway()

		ready := make(chan struct{})
		proceed := make(chan struct{})
		f.script(fake.Script{Hold: true, Cancelled: fake.Terminal("stopped", nil, nil), Do: func(session harness.Session) {
			g.requests(1)(session)
			close(ready)
			<-proceed
			g.requests(1)(session)
		}})

		runner := f.runner(g.options, func(o *run.Options) {
			// A TTL the spec can outlive, and a heartbeat frequent enough
			// to notice quickly once it has, matching the self-reclaim
			// spec in run_test.go.
			o.TTL = 12 * time.Second
			o.Interval = 200 * time.Millisecond
		})
		done := make(chan error, 1)
		go func() { done <- runner.Execute(ctx, f.nodeRef) }()

		By("when the step makes a request, its lease is reclaimed mid-attempt, and it makes another")
		Eventually(ready, 30*time.Second).Should(BeClosed())
		first := f.leaseOf(ctx).GetId()
		Expect(first).NotTo(BeEmpty())

		// Force the expiry but let nobody else claim the node: the next
		// heartbeat wins it back rather than fencing the step.
		f.expire(ctx, first)
		Eventually(func() string {
			return f.leaseOf(ctx).GetId()
		}, 30*time.Second, 200*time.Millisecond).ShouldNot(Or(BeEmpty(), Equal(first)))

		close(proceed)
		Eventually(func() int { return len(g.seen()) }, 10*time.Second, 100*time.Millisecond).Should(Equal(2))

		Expect(f.cancelRun(ctx)).To(Succeed())
		Eventually(done, 20*time.Second).Should(Receive(BeNil()))

		By("then both requests' usage reached the ledger, not just the one made before the reclaim")
		detail := f.inspect(ctx)
		attempts := stepRuns(detail, "gather-context")
		Expect(attempts).To(HaveLen(1))
		var gathered *millv1.StepRunDetail
		for _, d := range detail.GetSteps() {
			if d.GetStepRun().GetId() == attempts[0].GetId() {
				gathered = d
			}
		}
		Expect(gathered).NotTo(BeNil())
		// TotalsForStepRun groups by (model, source); both requests share
		// turnModel, so a fix is proven by the doubled counts in a single
		// row, not by a second row appearing.
		Expect(gathered.GetGatewayTotals()).To(HaveLen(1))
		counted := gathered.GetGatewayTotals()[0]
		Expect(counted.GetSource()).To(Equal(millv1.UsageSource_USAGE_SOURCE_GATEWAY))
		Expect(counted.GetModel()).To(Equal(turnModel))
		Expect(counted.GetInputTokens()).To(Equal(int64(2 * turnInput)))
		Expect(counted.GetOutputTokens()).To(Equal(int64(2 * turnOutput)))
		Expect(counted.GetCacheReadTokens()).To(Equal(int64(2 * turnCacheRead)))
		Expect(counted.GetCacheWriteTokens()).To(Equal(int64(2 * turnCacheWrite)))
	})
})
