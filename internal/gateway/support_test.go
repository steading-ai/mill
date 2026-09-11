package gateway_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/workspace"
)

// The credentials the specs hand around. Long enough for a grep of every
// sink to mean something, and unlike anything else the specs print.
const (
	// model is what the stand-in upstream answers as: the resolved id, as
	// the API reports it, rather than the alias a request names.
	model        = "claude-sonnet-4-5-20250929"
	subscription = "sk-ant-oat01-subscription-credential-spec-value"
	stepToken1   = "step-token-01JSPEC-ONE-bearer-value"
	stepToken2   = "step-token-01JSPEC-TWO-bearer-value"
)

// upstream is a stand-in for api.anthropic.com: it records every request it
// was handed and answers with what the spec scripted.
type upstream struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	answer   func(w http.ResponseWriter, r *http.Request)
}

// sseAnswer is a streamed Messages response as the API sends one: the usage
// block on message_start and again, final, on message_delta; padded data
// lines as the API pads them.
func sseAnswer(input, output, cacheRead, cacheWrite int64) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":%q,\"id\":\"msg_spec\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":%d,\"cache_creation_input_tokens\":%d,\"cache_read_input_tokens\":%d,\"output_tokens\":1}}     }\n\n",
				model, input, cacheWrite, cacheRead),
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"} }\n\n",
			"event: ping\ndata: {\"type\": \"ping\"}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}    }\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0 }\n\n",
			fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":%d,\"cache_creation_input_tokens\":%d,\"cache_read_input_tokens\":%d,\"output_tokens\":%d}   }\n\n",
				input, cacheWrite, cacheRead, output),
			"event: message_stop\ndata: {\"type\":\"message_stop\"   }\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// jsonAnswer is a non-streamed Messages response.
func jsonAnswer(input, output int64) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":%q,"id":"msg_spec","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":%d,"output_tokens":%d}}`, model, input, output)
	}
}

// newUpstream starts a stand-in answering as scripted.
func newUpstream(answer func(w http.ResponseWriter, r *http.Request)) *upstream {
	u := &upstream{answer: answer}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r.Clone(context.Background()))
		u.bodies = append(u.bodies, string(body))
		u.mu.Unlock()
		u.answer(w, r)
	}))
	DeferCleanup(u.server.Close)
	return u
}

// seen is every request the upstream was handed, in order.
func (u *upstream) seen() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request{}, u.requests...)
}

// headersDump is every header of every upstream request, flattened, so a
// grep for a token covers all of them.
func (u *upstream) headersDump() string {
	var b strings.Builder
	for _, r := range u.seen() {
		for name, values := range r.Header {
			fmt.Fprintf(&b, "%s: %s\n", name, strings.Join(values, ", "))
		}
		fmt.Fprintf(&b, "host: %s\n", r.Host)
	}
	return b.String()
}

// recorded is what the spec's recorder was handed.
type recorded struct {
	mu   sync.Mutex
	rows []gateway.Usage
}

func (r *recorded) record(_ context.Context, usage gateway.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, usage)
	return nil
}

func (r *recorded) all() []gateway.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]gateway.Usage{}, r.rows...)
}

// world is one spec's gateway: the stand-in upstream, the gateway in front
// of it on a real listener, its log, and the rows it recorded.
type world struct {
	upstream *upstream
	gateway  *gateway.Gateway
	server   *gateway.Server
	log      *bytes.Buffer
	rows     *recorded
}

func newWorld(answer func(w http.ResponseWriter, r *http.Request)) *world {
	GinkgoHelper()
	w := &world{upstream: newUpstream(answer), log: &bytes.Buffer{}, rows: &recorded{}}
	var err error
	w.gateway, err = gateway.New(gateway.Config{
		Upstream:   w.upstream.server.URL,
		Credential: gateway.CredentialOf(subscription),
		Log:        slog.New(slog.NewTextHandler(w.log, nil)),
	})
	Expect(err).NotTo(HaveOccurred())
	w.server, err = gateway.Listen("127.0.0.1:0", w.gateway)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(w.server.Close)
	return w
}

// step installs an attempt of the one run the specs drive, with the spec's
// recorder.
func (w *world) step(stepRunID, name, bearer string, budget int64, models ...string) {
	w.gateway.Rotate(runID, gateway.Step{
		StepRunID: stepRunID,
		StepName:  name,
		Token:     workspace.TokenFor(bearer),
		Budget:    budget,
		Models:    models,
		Record:    w.rows.record,
	})
}

// runID is the run every spec's attempts belong to.
const runID = "01RUN"

// request is a Messages request through the gateway as a session makes one:
// the CLI's own headers, the bearer, a body naming the model.
func (w *world) request(bearer, model string, stream bool) (*http.Response, string) {
	GinkgoHelper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":%t,"messages":[{"role":"user","content":"reply ok"}]}`, model, stream)
	req, err := http.NewRequest(http.MethodPost, w.server.URL()+"/v1/messages?beta=true", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14,context-1m-2025-08-07")
	req.Header.Set("User-Agent", "claude-cli/2.1.266 (external, cli)")
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return resp, string(raw)
}

// expected renders what the scripted upstream answer writes, for the
// byte-for-byte comparison.
func expected(answer func(w http.ResponseWriter, r *http.Request)) string {
	rec := httptest.NewRecorder()
	answer(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	return rec.Body.String()
}
