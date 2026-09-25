package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/steading-ai/mill/internal/workspace"
)

// DefaultListen is the address the gateway listens on when configuration
// names none: the host side of the machine network. On `core` that is the
// `mill-host` dummy link homestead's `mill_egress` role creates, and its
// port is the one besides squid's that the `mill` firewalld zone and the
// `inet mill` table admit from `ve-mill*` (docs/sandbox.md §Network).
const DefaultListen = "10.99.255.1:8080"

// DefaultUpstream is the API the gateway forwards to when configuration
// names no other.
const DefaultUpstream = "https://api.anthropic.com"

// The Messages API, which is all the gateway forwards. `?beta=true` and the
// like are the client's to add; the path is what is checked.
const (
	messagesPath    = "/v1/messages"
	countTokensPath = "/v1/messages/count_tokens"
)

// maxRequestBody bounds a request the gateway reads to find the model. A
// session's context is megabytes; a body past this is not a Messages
// request the gateway will forward.
const maxRequestBody = 64 << 20

// recordTimeout bounds the ledger write a response's usage becomes. It runs
// after the client has its bytes, on a context of its own, so a slow store
// never holds a stream open.
const recordTimeout = 30 * time.Second

// Recorder is where a response's usage goes: the run loop's ledger write,
// under the run's lease, with source `gateway`.
type Recorder func(ctx context.Context, usage Usage) error

// Step is what the gateway authenticates and accounts against: one attempt,
// its token, and what its workflow declared. The run loop installs one with
// [Gateway.Rotate] beside the workspace's own rotation and removes it with
// [Gateway.Revoke] when the attempt ends, so the gateway checks the token
// the store issued last and refuses the one before it.
type Step struct {
	// RunID is the run the attempt belongs to. One step is live per run.
	RunID string
	// StepRunID is the attempt, which usage is recorded against.
	StepRunID string
	// StepName is the step, for the log and for a refusal's message.
	StepName string
	// Token is the step token, compared by hash exactly as the workspace
	// socket compares it.
	Token workspace.Token
	// Budget is how many tokens the attempt may bill before its requests
	// are refused; zero declares none (mill.v1.Step.token_budget).
	Budget int64
	// Models is the workflow's pin; empty admits every model
	// (mill.v1.Step.models).
	Models []string
	// Record is where each response's usage goes. Nil records nothing.
	Record Recorder
}

// live is a step as the gateway holds it, with what it has billed so far.
type live struct {
	step     Step
	consumed int64
	refused  *BudgetError
}

// BudgetError is a request refused because the attempt has spent its
// budget. It is what the gateway answers with, and what the run loop fails
// the attempt on.
type BudgetError struct {
	// StepName is the step whose budget it is.
	StepName string
	// StepRunID is the attempt refused.
	StepRunID string
	// Budget is the workflow's number.
	Budget int64
	// Consumed is what the attempt had billed when it was refused.
	Consumed int64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("step %s has used %d of its token budget of %d tokens, and the gateway refused its next request",
		e.StepName, e.Consumed, e.Budget)
}

// ModelError is a request naming a model the workflow did not pin.
type ModelError struct {
	// StepName is the step whose pin it is.
	StepName string
	// Model is what the request asked for.
	Model string
	// Pinned is what the workflow allows.
	Pinned []string
}

func (e *ModelError) Error() string {
	return fmt.Sprintf("model %s is not pinned by step %s, whose workflow admits %s",
		e.Model, e.StepName, strings.Join(e.Pinned, ", "))
}

// Config is what a gateway is built from.
type Config struct {
	// Upstream is where requests go. Empty means [DefaultUpstream].
	Upstream string
	// Credential is the bearer presented upstream in place of the step
	// token. It is loaded from a file and never from the environment.
	Credential Credential
	// Transport reaches the upstream. Nil means the default transport.
	Transport http.RoundTripper
	// Log is where one line per request goes. Nil discards them. Nothing
	// written here carries a token or the credential: tokens are named by
	// their id and the credential by its fingerprint.
	Log *slog.Logger
}

// Gateway is the reverse proxy. It is an http.Handler; [Listen] puts it on
// an address.
type Gateway struct {
	upstream   *url.URL
	credential Credential
	log        *slog.Logger
	proxy      *httputil.ReverseProxy

	mu    sync.Mutex
	steps map[string]*live
}

// New builds a gateway.
func New(config Config) (*Gateway, error) {
	if config.Upstream == "" {
		config.Upstream = DefaultUpstream
	}
	upstream, err := url.Parse(config.Upstream)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("gateway: upstream %q is not a URL", config.Upstream)
	}
	if config.Log == nil {
		config.Log = slog.New(slog.DiscardHandler)
	}
	g := &Gateway{
		upstream:   upstream,
		credential: config.Credential,
		log:        config.Log,
		steps:      map[string]*live{},
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:   g.rewrite,
		Transport: config.Transport,
		// Every byte is written as it arrives: a streamed response is
		// the session's model speaking, and buffering it is latency.
		FlushInterval:  -1,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.upstreamError,
		ErrorLog:       slog.NewLogLogger(config.Log.Handler(), slog.LevelWarn),
	}
	return g, nil
}

// Rotate installs the step the run's requests authenticate as from now on.
// Whatever the run had installed before stops working at once, with its
// accounting.
func (g *Gateway) Rotate(runID string, step Step) {
	g.mu.Lock()
	defer g.mu.Unlock()
	step.RunID = runID
	if !step.Token.Live() {
		delete(g.steps, runID)
		return
	}
	g.steps[runID] = &live{step: step}
}

// Revoke removes the run's step. Its token is refused from now on.
func (g *Gateway) Revoke(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.steps, runID)
}

// Exceeded reports the refusal an attempt earned by spending its budget, or
// nil. The run loop reads it when the session ends, before it revokes.
func (g *Gateway) Exceeded(stepRunID string) *BudgetError {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.steps {
		if l.step.StepRunID == stepRunID {
			return l.refused
		}
	}
	return nil
}

// Consumed is what an attempt has billed through the gateway so far, in
// tokens.
func (g *Gateway) Consumed(stepRunID string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.steps {
		if l.step.StepRunID == stepRunID {
			return l.consumed
		}
	}
	return 0
}

// authenticate finds the live step a bearer belongs to.
func (g *Gateway) authenticate(bearer string) *live {
	if bearer == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.steps {
		if l.step.Token.Matches(bearer) {
			return l
		}
	}
	return nil
}

// stepKey is the context key the live step travels to the proxy's hooks
// under.
type stepKey struct{}

// ServeHTTP is the gateway: refuse what is not the Messages API, authenticate
// the bearer, apply the pin and the budget, swap the credential, forward.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || (r.URL.Path != messagesPath && r.URL.Path != countTokensPath) {
		g.log.Info("gateway: refused", "reason", "not the Messages API", "method", r.Method, "path", r.URL.Path)
		refuse(w, http.StatusNotFound, "not_found_error", "the mill's credential gateway forwards the Messages API and nothing else")
		return
	}
	l := g.authenticate(bearerOf(r))
	if l == nil {
		g.log.Info("gateway: refused", "reason", "the bearer is not a live step token", "path", r.URL.Path)
		refuse(w, http.StatusUnauthorized, "authentication_error", "the bearer is not the live step token of any run on this worker")
		return
	}
	step := l.step
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		refuse(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the request body could not be read")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	model := modelOf(body)

	if err := pinned(step, model); err != nil {
		g.log.Info("gateway: refused", "reason", "unpinned model", "step", step.StepName, "step_run", step.StepRunID, "model", model)
		refuse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if over := g.overBudget(l); over != nil {
		g.log.Info("gateway: refused", "reason", "budget", "step", step.StepName, "step_run", step.StepRunID, "budget", over.Budget, "consumed", over.Consumed)
		refuse(w, http.StatusTooManyRequests, "rate_limit_error", over.Error())
		return
	}
	g.log.Info("gateway: forwarding",
		"step", step.StepName, "step_run", step.StepRunID, "token", tokenName(step.Token),
		"model", model, "path", r.URL.Path, "upstream", g.upstream.Host, "credential", g.credential)
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stepKey{}, l)))
}

// overBudget applies the budget once the attempt has spent it, and keeps
// the refusal for the run loop.
func (g *Gateway) overBudget(l *live) *BudgetError {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l.step.Budget <= 0 || l.consumed < l.step.Budget {
		return nil
	}
	if l.refused == nil {
		l.refused = &BudgetError{
			StepName:  l.step.StepName,
			StepRunID: l.step.StepRunID,
			Budget:    l.step.Budget,
			Consumed:  l.consumed,
		}
	}
	return l.refused
}

// rewrite is the outbound request: the upstream's address and host, the
// credential in place of the step token, and the client's own headers
// otherwise — the CLI's `anthropic-beta` and `user-agent` among them, which
// the upstream wants unchanged (docs/spikes/nspawn-per-run-sandbox.md
// §E14). Accept-Encoding is dropped so the transport negotiates and
// decodes any compression itself, which is what lets the usage be read off
// the bytes the client is given.
func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	pr.Out.URL.Scheme = g.upstream.Scheme
	pr.Out.URL.Host = g.upstream.Host
	pr.Out.URL.Path = strings.TrimSuffix(g.upstream.Path, "/") + pr.In.URL.Path
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery
	pr.Out.Host = g.upstream.Host
	pr.Out.Header.Set("Authorization", "Bearer "+g.credential.value)
	pr.Out.Header.Del("Accept-Encoding")
}

// modifyResponse puts the observer on a forwarded response, so that what
// the upstream billed is counted as the bytes pass to the client.
func (g *Gateway) modifyResponse(resp *http.Response) error {
	l, _ := resp.Request.Context().Value(stepKey{}).(*live)
	if l == nil || resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, messagesPath) {
		return nil
	}
	streaming := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	resp.Body = newObserver(resp.Body, streaming, func(usage Usage, seen bool) {
		if !seen {
			g.log.Warn("gateway: no usage on the response", "step", l.step.StepName, "step_run", l.step.StepRunID)
			return
		}
		g.count(l, usage)
	})
	return nil
}

// count records one response's usage: in the attempt's running total, and
// in the ledger through the step's recorder.
func (g *Gateway) count(l *live, usage Usage) {
	g.mu.Lock()
	l.consumed += usage.Tokens()
	consumed := l.consumed
	g.mu.Unlock()
	g.log.Info("gateway: usage",
		"step", l.step.StepName, "step_run", l.step.StepRunID, "model", usage.Model,
		"input", usage.Input, "output", usage.Output, "cache_read", usage.CacheRead, "cache_write", usage.CacheWrite,
		"consumed", consumed)
	if l.step.Record == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := l.step.Record(ctx, usage); err != nil {
		g.log.Error("gateway: record usage", "step", l.step.StepName, "step_run", l.step.StepRunID, "error", err)
	}
}

// upstreamError answers for an upstream that could not be reached. A client
// that went away mid-stream is not one: the proxy stops and nothing is
// written.
func (g *Gateway) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	g.log.Warn("gateway: upstream", "upstream", g.upstream.Host, "path", r.URL.Path, "error", err)
	refuse(w, http.StatusBadGateway, "api_error", "the upstream could not be reached through the mill's credential gateway")
}

// pinned applies the workflow's model pin. An entry matches a model it
// equals or that contains it, so `sonnet` pins every Sonnet.
func pinned(step Step, model string) error {
	if len(step.Models) == 0 {
		return nil
	}
	for _, pin := range step.Models {
		if model == pin || strings.Contains(model, pin) {
			return nil
		}
	}
	return &ModelError{StepName: step.StepName, Model: model, Pinned: step.Models}
}

// modelOf reads the model a Messages request names. A body that is not a
// document names none, and the pin refuses it when there is one.
func modelOf(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	return request.Model
}

// bearerOf is the request's bearer, or empty.
func bearerOf(r *http.Request) string {
	scheme, value, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// tokenName is how a step token is named in the log: its id, or when the
// store gave none, a fingerprint of its hash. Never the bearer.
func tokenName(token workspace.Token) string {
	if token.ID != "" {
		return token.ID
	}
	return "hash:" + fingerprint(token.Hash)
}

// refuse writes an error in the API's own shape, so a client that reads
// error bodies reads this one.
func refuse(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}

// Server is a listening gateway.
type Server struct {
	gateway  *Gateway
	listener net.Listener
	server   *http.Server
}

// Listen puts the gateway on addr and serves until Close. The address a
// session is pointed at is [Server.URL].
func Listen(addr string, g *Gateway) (*Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("gateway: listen on %s: %w", addr, err)
	}
	s := &Server{
		gateway:  g,
		listener: listener,
		server: &http.Server{
			Handler:           g,
			ReadHeaderTimeout: 30 * time.Second,
		},
	}
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

// Gateway is the handler being served.
func (s *Server) Gateway() *Gateway { return s.gateway }

// URL is what ANTHROPIC_BASE_URL is set to for a session that reaches this
// server.
func (s *Server) URL() string { return "http://" + s.listener.Addr().String() }

// Close stops serving. In-flight streams are cut.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}
