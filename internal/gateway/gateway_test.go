package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/mill/internal/gateway"
	"github.com/steading-ai/mill/internal/workspace"
)

// The credential gateway of docs/sandbox.md §Credentials: the session holds
// the step token, the host holds the subscription, and the gateway between
// them swaps one for the other, counts what the model billed, and applies
// the workflow's budget and pin.

var _ = Describe("The credential gateway", func() {
	It("The session's requests carry the step token and the upstream sees the subscription", func() {
		By("given a gateway in front of a fake upstream that streams a response")
		answer := sseAnswer(12, 7, 300, 40)
		w := newWorld(answer)
		w.step("01STEP1", "implement", stepToken1, 0)

		By("when a session makes a model request bearing the current step token")
		resp, body := w.request(stepToken1, model, true)

		By("then the gateway receives the step token and forwards with the upstream credential")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		seen := w.upstream.seen()
		Expect(seen).To(HaveLen(1))
		Expect(seen[0].Header.Get("Authorization")).To(Equal("Bearer " + subscription))

		By("and the upstream receives the subscription credential and not the step token")
		Expect(w.upstream.headersDump()).NotTo(ContainSubstring(stepToken1))
		Expect(w.upstream.bodies[0]).NotTo(ContainSubstring(stepToken1))

		By("and the CLI's own headers reach the upstream unchanged")
		Expect(seen[0].Header.Get("anthropic-beta")).To(Equal("interleaved-thinking-2025-05-14,context-1m-2025-08-07"))
		Expect(seen[0].Header.Get("User-Agent")).To(Equal("claude-cli/2.1.266 (external, cli)"))
		Expect(seen[0].Header.Get("anthropic-version")).To(Equal("2023-06-01"))
		Expect(seen[0].URL.RawQuery).To(Equal("beta=true"))

		By("and the streamed response reaches the client byte-for-byte")
		Expect(resp.Header.Get("Content-Type")).To(HavePrefix("text/event-stream"))
		Expect(body).To(Equal(expected(answer)))
	})

	It("A rotated step token is refused", func() {
		w := newWorld(jsonAnswer(1, 1))

		By("given step 1's token was in use")
		w.step("01STEP1", "gather-context", stepToken1, 0)
		resp, _ := w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("when Rotate installs step 2's token and a request with step 1's arrives")
		w.step("01STEP2", "implement", stepToken2, 0)
		resp, body := w.request(stepToken1, model, false)

		By("then the gateway refuses it with 401")
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(body).To(ContainSubstring("authentication_error"))
		Expect(w.upstream.seen()).To(HaveLen(1), "the refused request never reached the upstream")

		By("and the new token is accepted")
		resp, _ = w.request(stepToken2, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(w.upstream.seen()).To(HaveLen(2))

		By("and a token nobody issued is refused too")
		resp, _ = w.request("never-issued", model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		req, err := http.NewRequest(http.MethodPost, w.server.URL()+"/v1/messages", strings.NewReader(`{}`))
		Expect(err).NotTo(HaveOccurred())
		bare, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = bare.Body.Close()
		Expect(bare.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("Usage is recorded from outside the session", func() {
		By("given a gateway whose upstream answers for step 3 with model M")
		w := newWorld(sseAnswer(12, 7, 300, 40))
		w.step("01STEP3", "implement", stepToken1, 0)

		By("when a response passes through the gateway for step 3 with model M")
		resp, _ := w.request(stepToken1, "claude-sonnet-4-5", true)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("then the ledger holds a usage row for step 3 and M from the gateway, whose counts match the upstream's usage block")
		Eventually(w.rows.all).Should(HaveLen(1))
		Expect(w.rows.all()[0]).To(Equal(gateway.Usage{Model: model, Input: 12, Output: 7, CacheRead: 300, CacheWrite: 40}))
		Expect(w.gateway.Consumed("01STEP3")).To(Equal(int64(12 + 7 + 300 + 40)))

		By("and a non-streamed response is counted the same way")
		w2 := newWorld(jsonAnswer(5, 3))
		w2.step("01STEP3", "implement", stepToken1, 0)
		resp, _ = w2.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Eventually(w2.rows.all).Should(ConsistOf(gateway.Usage{Model: model, Input: 5, Output: 3}))
	})

	It("A step over its budget is refused", func() {
		By("given step 3 has a token budget of 20 and has consumed it")
		w := newWorld(sseAnswer(8, 4, 6, 2))
		w.step("01STEP3", "implement", stepToken1, 20)
		resp, _ := w.request(stepToken1, model, true)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Eventually(func() int64 { return w.gateway.Consumed("01STEP3") }).Should(Equal(int64(20)))
		Expect(w.gateway.Exceeded("01STEP3")).To(BeNil(), "nothing has been refused yet")

		By("when the session makes another request")
		resp, body := w.request(stepToken1, model, true)

		By("then the gateway refuses with 429 naming the budget")
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		var refusal struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		Expect(json.Unmarshal([]byte(body), &refusal)).To(Succeed())
		Expect(refusal.Error.Type).To(Equal("rate_limit_error"))
		Expect(refusal.Error.Message).To(ContainSubstring("token budget of 20 tokens"))
		Expect(refusal.Error.Message).To(ContainSubstring("step implement has used 20"))
		Expect(w.upstream.seen()).To(HaveLen(1), "the refused request never reached the upstream")

		By("and the step fails with the budget as evidence: the run loop reads the refusal off the gateway")
		over := w.gateway.Exceeded("01STEP3")
		Expect(over).NotTo(BeNil())
		Expect(over.Budget).To(Equal(int64(20)))
		Expect(over.Consumed).To(Equal(int64(20)))
		Expect(over.StepRunID).To(Equal("01STEP3"))
		Expect(over.Error()).To(ContainSubstring("token budget of 20"))

		By("and the gateway's own budget refusal is never mistaken for the upstream's")
		Expect(w.gateway.Refused("01STEP3")).To(BeNil())
		Expect(w.gateway.Outage()).To(BeNil())
	})

	It("An unpinned model is refused", func() {
		By("given a step whose workflow pins sonnet and haiku")
		w := newWorld(jsonAnswer(1, 1))
		w.step("01STEP3", "implement", stepToken1, 0, "sonnet", "claude-haiku-4-5")

		By("when the session asks for an opus model")
		resp, body := w.request(stepToken1, "claude-opus-4-1", false)

		By("then the gateway refuses with 400 naming the pin")
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(body).To(ContainSubstring("invalid_request_error"))
		Expect(body).To(ContainSubstring("claude-opus-4-1 is not pinned by step implement"))
		Expect(body).To(ContainSubstring("sonnet, claude-haiku-4-5"))
		Expect(w.upstream.seen()).To(BeEmpty())

		By("and a pinned model, by family or by id, goes through")
		resp, _ = w.request(stepToken1, "claude-sonnet-4-5-20250929", false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		resp, _ = w.request(stepToken1, "claude-haiku-4-5", false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("Paths outside the Messages API are refused", func() {
		w := newWorld(jsonAnswer(1, 1))
		w.step("01STEP1", "implement", stepToken1, 0)
		for _, probe := range []struct{ method, path string }{
			{http.MethodGet, "/v1/models"},
			{http.MethodGet, "/v1/messages"},
			{http.MethodPost, "/v1/complete"},
			{http.MethodPost, "/"},
		} {
			req, err := http.NewRequest(probe.method, w.server.URL()+probe.path, strings.NewReader(`{}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+stepToken1)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound), "%s %s", probe.method, probe.path)
		}
		Expect(w.upstream.seen()).To(BeEmpty())
	})

	It("The token never appears in logs, the ledger, or error bodies", func() {
		By("given a gateway that has forwarded, refused a rotated token, refused a model and refused a budget")
		w := newWorld(sseAnswer(8, 4, 6, 2))
		w.step("01STEP1", "implement", stepToken1, 20)
		var bodies []string
		collect := func(resp *http.Response, body string) {
			bodies = append(bodies, body)
			for name, values := range resp.Header {
				bodies = append(bodies, name+": "+strings.Join(values, ","))
			}
		}
		collect(w.request(stepToken1, model, true))
		Eventually(func() int64 { return w.gateway.Consumed("01STEP1") }).Should(Equal(int64(20)))
		collect(w.request(stepToken1, model, true))
		collect(w.request(stepToken2, model, true))
		w.step("01STEP2", "review", stepToken2, 0, "haiku")
		collect(w.request(stepToken1, model, true))
		collect(w.request(stepToken2, model, true))
		w.gateway.Revoke(runID)
		collect(w.request(stepToken2, model, true))

		By("then neither step token nor the subscription credential is in the log")
		Eventually(func() string { return w.log.String() }).Should(ContainSubstring("gateway: usage"))
		for _, secret := range []string{stepToken1, stepToken2, subscription} {
			Expect(w.log.String()).NotTo(ContainSubstring(secret))
			By("nor in any response body or header")
			Expect(strings.Join(bodies, "\n")).NotTo(ContainSubstring(secret))
		}
		Expect(w.log.String()).To(ContainSubstring("gateway: forwarding"))
		Expect(w.log.String()).To(ContainSubstring("gateway: refused"))

		By("and the ledger rows carry counts and a model, nothing else")
		rows := w.rows.all()
		Expect(rows).NotTo(BeEmpty())
		for _, row := range rows {
			Expect(row.Model).To(Equal(model))
		}

		By("and the credential renders as its fingerprint wherever it is printed")
		credential := gateway.CredentialOf(subscription)
		Expect(credential.String()).NotTo(ContainSubstring(subscription))
		Expect(credential.GoString()).NotTo(ContainSubstring(subscription))
		Expect(credential.Fingerprint()).To(HaveLen(8))
	})

	It("An abandoned stream still counts what the upstream had said", func() {
		// The CLI opens a parallel title request and drops it midway; the
		// gateway records what message_start had said and treats the
		// close as the end of that response, not as an upstream failure.
		w := newWorld(sseAnswer(12, 7, 300, 40))
		w.step("01STEP1", "implement", stepToken1, 0)
		req, err := http.NewRequest(http.MethodPost, w.server.URL()+"/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[]}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+stepToken1)
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		Expect(err).NotTo(HaveOccurred())
		buf := make([]byte, 64)
		_, _ = resp.Body.Read(buf)
		cancel()
		_ = resp.Body.Close()
		Eventually(w.rows.all).Should(HaveLen(1))
		Expect(w.rows.all()[0].Model).To(Equal(model))
		Expect(w.rows.all()[0].Input).To(Equal(int64(12)))
	})

	It("The doctor's probe goes through the gateway to the upstream", func() {
		By("given an upstream that answers a token count")
		w := newWorld(func(rw http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/v1/messages/count_tokens"))
			Expect(r.Header.Get("Authorization")).To(Equal("Bearer " + subscription))
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"input_tokens":8}`))
		})

		By("then the probe passes, and the token it minted is gone afterwards")
		Expect(w.gateway.Probe(context.Background())).To(Succeed())
		Expect(w.upstream.seen()).To(HaveLen(1))
		Expect(w.upstream.headersDump()).NotTo(ContainSubstring("probe-"))
		Expect(w.rows.all()).To(BeEmpty(), "a token count is not usage")

		By("and it fails naming the upstream's answer when the credential is refused")
		refusing := newWorld(func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid bearer token"}}`))
		})
		err := refusing.gateway.Probe(context.Background())
		Expect(err).To(MatchError(ContainSubstring("answered 401")))
		Expect(err.Error()).NotTo(ContainSubstring(subscription))

		By("and without a credential it does not try")
		none, err := gateway.New(gateway.Config{Upstream: w.upstream.server.URL})
		Expect(err).NotTo(HaveOccurred())
		Expect(none.Probe(context.Background())).To(MatchError(gateway.ErrNoCredential))
	})

	It("An upstream refusal is observed with the reset it names", func() {
		By("given the upstream refuses with retry-after in seconds")
		before := time.Now()
		w := newWorld(func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Retry-After", "20")
			rw.WriteHeader(http.StatusTooManyRequests)
		})
		w.step("01STEP1", "implement", stepToken1, 0)
		resp, _ := w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))

		refusal := w.gateway.Refused("01STEP1")
		Expect(refusal).NotTo(BeNil())
		Expect(refusal.Status).To(Equal(http.StatusTooManyRequests))
		Expect(refusal.Reset).To(BeTemporally("~", before.Add(20*time.Second), 5*time.Second))
		Expect(w.gateway.Outage()).To(Equal(refusal))

		By("and a reset named by a unified rate-limit header instead")
		later := before.Add(time.Hour).UTC().Truncate(time.Second)
		w2 := newWorld(func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("anthropic-ratelimit-tokens-reset", later.Format(time.RFC3339))
			rw.WriteHeader(http.StatusTooManyRequests)
		})
		w2.step("01STEP1", "implement", stepToken1, 0)
		resp, _ = w2.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(w2.gateway.Refused("01STEP1").Reset).To(BeTemporally("==", later))

		By("and a refusal naming neither header leaves the reset unknown")
		w3 := newWorld(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusTooManyRequests) })
		w3.step("01STEP1", "implement", stepToken1, 0)
		resp, _ = w3.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(w3.gateway.Refused("01STEP1").Reset).To(BeZero())

		By("and a 5xx is an outage the same way a 429 is")
		w4 := newWorld(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusServiceUnavailable) })
		w4.step("01STEP1", "implement", stepToken1, 0)
		resp, _ = w4.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(w4.gateway.Refused("01STEP1").Status).To(Equal(http.StatusServiceUnavailable))
	})

	It("An unreachable upstream is an outage too", func() {
		By("given a gateway pointed at nothing listening")
		closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := closed.URL
		closed.Close()
		log := &bytes.Buffer{}
		gw, err := gateway.New(gateway.Config{
			Upstream:   addr,
			Credential: gateway.CredentialOf(subscription),
			Log:        slog.New(slog.NewTextHandler(log, nil)),
		})
		Expect(err).NotTo(HaveOccurred())
		srv, err := gateway.Listen("127.0.0.1:0", gw)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(srv.Close)
		gw.Rotate(runID, gateway.Step{StepRunID: "01STEP1", StepName: "implement", Token: workspace.TokenFor(stepToken1)})

		By("when a request is made")
		req, err := http.NewRequest(http.MethodPost, srv.URL()+"/v1/messages", strings.NewReader(`{"model":"`+model+`"}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+stepToken1)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()

		By("then it is recorded as a refusal with no status and no reset")
		Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		refusal := gw.Refused("01STEP1")
		Expect(refusal).NotTo(BeNil())
		Expect(refusal.Status).To(Equal(0))
		Expect(refusal.Reset).To(BeZero())
		Expect(gw.Outage()).NotTo(BeNil())
	})

	It("A success clears the outage, whichever step it comes from", func() {
		By("given step A's request is refused")
		var refuse atomic.Bool
		refuse.Store(true)
		w := newWorld(toggledRefusal(&refuse))
		w.step("01STEPA", "implement", stepToken1, 0)
		resp, _ := w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(w.gateway.Outage()).NotTo(BeNil())

		By("when that same step succeeds")
		refuse.Store(false)
		resp, _ = w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(w.gateway.Refused("01STEPA")).To(BeNil())
		Expect(w.gateway.Outage()).To(BeNil())
	})

	It("An unrelated step's success clears an outage it never saw", func() {
		By("given step A's request is refused")
		var refuse atomic.Bool
		refuse.Store(true)
		w := newWorld(toggledRefusal(&refuse))
		w.step("01STEPA", "implement", stepToken1, 0)
		resp, _ := w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(w.gateway.Outage()).NotTo(BeNil())

		By("when an unrelated step's request succeeds")
		refuse.Store(false)
		w.step("01STEPB", "review", stepToken2, 0)
		resp, _ = w.request(stepToken2, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("then the process-wide outage is cleared even though step B never saw a refusal")
		Expect(w.gateway.Outage()).To(BeNil())
	})

	It("The doctor's probe clears an outage another step observed", func() {
		By("given a step's request is refused while count_tokens still answers")
		w := newWorld(func(rw http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/messages/count_tokens" {
				rw.Header().Set("Content-Type", "application/json")
				_, _ = rw.Write([]byte(`{"input_tokens":8}`))
				return
			}
			rw.WriteHeader(http.StatusTooManyRequests)
		})
		w.step("01STEP1", "implement", stepToken1, 0)
		resp, _ := w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(w.gateway.Outage()).NotTo(BeNil())

		By("when the probe succeeds")
		Expect(w.gateway.Probe(context.Background())).To(Succeed())

		By("then the outage is cleared, though the probe's own live never itself saw the refusal")
		Expect(w.gateway.Outage()).To(BeNil())
	})

	It("A streak of refusals logs once, and recovery logs once", func() {
		var refuse atomic.Bool
		refuse.Store(true)
		w := newWorld(toggledRefusal(&refuse))
		w.step("01STEP1", "implement", stepToken1, 0)

		for range 3 {
			resp, _ := w.request(stepToken1, model, false)
			Expect(resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		}
		Expect(strings.Count(w.log.String(), "gateway: upstream refusing")).To(Equal(1))

		refuse.Store(false)
		resp, _ := w.request(stepToken1, model, false)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(strings.Count(w.log.String(), "gateway: upstream recovered")).To(Equal(1))
	})
})

// toggledRefusal answers 429 while refuse holds true, and a bare successful
// Messages response once it is flipped false — the fixture behind every spec
// in this file that needs an upstream it can cap and un-cap mid-test.
func toggledRefusal(refuse *atomic.Bool) func(http.ResponseWriter, *http.Request) {
	return func(rw http.ResponseWriter, _ *http.Request) {
		if refuse.Load() {
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"model":"` + model + `","id":"msg","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}
}
