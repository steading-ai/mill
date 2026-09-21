// Package dagger is mill's client for dagger's dagger.v1 Connect API: the
// only place in the mill that talks to the work graph.
//
// The generated client is dagger's own — proto/dagger/v1 is dagger's
// contract, vendored — so nothing here re-describes the wire. What it adds
// is the discipline docs/domain-model.md §Principal puts on mill and dagger
// does not enforce, because mill runs as the operator's `user` principal and
// dagger applies no gate veto to a user:
//
//   - Every claim carries `harness: mill`, an `agent_ref` naming the worker
//     and a `session_ref` naming the run, so "everything mill did" is a join
//     on the lease.
//   - Every ApplyPatch carries a `request_id` that begins `mill-`, which is
//     the only mark on a lease-less write that says mill made it.
//   - Mill never claims, completes, drops, reclassifies, or unlinks a node
//     whose assignee_kind is `user` — a gate, including one mill filed. The
//     refusal is here, at the seam, so no caller can route around it.
package dagger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	"github.com/ivy/mill/gen/dagger/v1/daggerv1connect"
)

// socketURL is the base every request is made against. The host is never
// resolved — the transport dials the unix socket whatever it says — but
// Connect needs a well-formed URL to build a path from.
const socketURL = "http://dagger"

// HarnessName is what mill puts in the `harness` field of every lease it
// takes. docs/domain-model.md §Principal calls it not optional.
const HarnessName = "mill"

// RequestIDPrefix begins every request id mill mints. On a lease-less write
// it is the only thing in dagger's event log that says mill did it.
const RequestIDPrefix = "mill-"

// Client is the mill's view of one dagger service.
type Client struct {
	graph   daggerv1connect.GraphServiceClient
	work    daggerv1connect.WorkServiceClient
	project string
	options options
}

type options struct {
	agentRef   string
	sessionRef string
	ttl        time.Duration
}

// Option configures a [Client].
type Option func(*options)

// WithAgentRef names the worker every claim is attributed to.
func WithAgentRef(ref string) Option { return func(o *options) { o.agentRef = ref } }

// WithSessionRef names the run every claim is attributed to.
func WithSessionRef(ref string) Option { return func(o *options) { o.sessionRef = ref } }

// WithTTL is the lease TTL every claim asks for. The server bounds it and
// applies its own default when this is zero.
func WithTTL(ttl time.Duration) Option { return func(o *options) { o.ttl = ttl } }

// Dial builds a client over dagger's unix socket. project is what a bare
// `#7` reference resolves in.
func Dial(socket, project string, opts ...Option) *Client {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	return New(client, socketURL, project, opts...)
}

// New builds a client over an HTTP client and base URL of the caller's
// choosing, which is how a spec points it at a service it started itself.
func New(httpClient connect.HTTPClient, baseURL, project string, opts ...Option) *Client {
	c := &Client{
		graph:   daggerv1connect.NewGraphServiceClient(httpClient, baseURL),
		work:    daggerv1connect.NewWorkServiceClient(httpClient, baseURL),
		project: project,
	}
	for _, opt := range opts {
		opt(&c.options)
	}
	return c
}

// Project is what a bare node reference resolves in.
func (c *Client) Project() string { return c.project }

// Scoped is a copy of c pinned to project, sharing the same connections. A
// worker claiming across every project builds one whenever a claim needs to
// write or read gates within the project the claim actually landed in,
// rather than the worker's own (possibly empty) one.
func (c *Client) Scoped(project string) *Client {
	scoped := *c
	scoped.project = project
	return &scoped
}

// Node reads one node in full: the body is the prompt a step is built from,
// so VIEW_FULL is the only view worth asking for here.
func (c *Client) Node(ctx context.Context, ref string) (*daggerv1.Node, error) {
	res, err := c.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: c.project,
		Node:    ref,
		View:    daggerv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, fmt.Errorf("dagger: get node %s: %w", ref, err)
	}
	return res.Msg.GetNode(), nil
}

// Result is what a node was completed with, or nil while it is open. It is
// read by reference like every other node read, and it is separate from
// [Client.Node] because the node message carries no result of its own.
func (c *Client) Result(ctx context.Context, ref string) (*daggerv1.Result, error) {
	res, err := c.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: c.project,
		Node:    ref,
		View:    daggerv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, wrap("dagger: get result of "+ref, err)
	}
	return res.Msg.GetResult(), nil
}

// Inputs are the results of the predecessors that satisfy a node. They are
// other principals' claims and are rendered into a prompt as attributed
// data, never as instructions (docs/architecture.md §"Trust boundaries").
func (c *Client) Inputs(ctx context.Context, ref string) ([]*daggerv1.Input, error) {
	res, err := c.graph.GetInputs(ctx, connect.NewRequest(&daggerv1.GetInputsRequest{
		Project: c.project,
		Node:    ref,
		View:    daggerv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, fmt.Errorf("dagger: get inputs of %s: %w", ref, err)
	}
	return res.Msg.GetInputs(), nil
}

// Explain says why a node is not ready, one sentence per condition that does
// not hold. An empty slice means it is ready.
func (c *Client) Explain(ctx context.Context, ref string) ([]string, error) {
	res, err := c.graph.Explain(ctx, connect.NewRequest(&daggerv1.ExplainRequest{
		Project: c.project,
		Node:    ref,
	}))
	if err != nil {
		return nil, fmt.Errorf("dagger: explain %s: %w", ref, err)
	}
	reasons := make([]string, 0, len(res.Msg.GetReasons()))
	for _, reason := range res.Msg.GetReasons() {
		reasons = append(reasons, describe(reason))
	}
	return reasons, nil
}

// describe renders one readiness reason as a sentence.
func describe(reason *daggerv1.Reason) string {
	switch r := reason.GetReason().(type) {
	case *daggerv1.Reason_NotOpen:
		return fmt.Sprintf("the node is %s, not open", statusName(r.NotOpen.GetStatus()))
	case *daggerv1.Reason_Blocked:
		what := "predecessor"
		if r.Blocked.GetGate() {
			what = "gate"
		}
		return fmt.Sprintf("blocked by %s %s", what, r.Blocked.GetBlocker().GetReference())
	case *daggerv1.Reason_Leased:
		return fmt.Sprintf("leased until %s", r.Leased.GetLease().GetExpiresAt().AsTime().Format(time.RFC3339))
	case *daggerv1.Reason_OpenChild:
		return fmt.Sprintf("open child %s", r.OpenChild.GetChild().GetReference())
	case *daggerv1.Reason_Archived:
		return fmt.Sprintf("project %s is archived", r.Archived.GetProject().GetSlug())
	case *daggerv1.Reason_Held:
		if r.Held.GetSelf() {
			return "the node is held"
		}
		return fmt.Sprintf("held by #%d %s", r.Held.GetNumber(), r.Held.GetTitle())
	case *daggerv1.Reason_ProjectHeld:
		return fmt.Sprintf("project %s is held", r.ProjectHeld.GetProject().GetSlug())
	default:
		return "not ready"
	}
}

// statusName is a dagger status without its enum prefix.
func statusName(status daggerv1.Status) string {
	switch status {
	case daggerv1.Status_STATUS_OPEN:
		return "open"
	case daggerv1.Status_STATUS_DONE:
		return "done"
	case daggerv1.Status_STATUS_DROPPED:
		return "dropped"
	default:
		return "unspecified"
	}
}

// RequestID is the idempotency key of one patch, in the shape docs/ledger.md
// §"What goes to dagger, and when" fixes: `mill-<run>-<step>-<attempt>`. The
// same step run retried after a crash sends the same key, so dagger applies
// the patch once.
func RequestID(runID, step string, attempt int64) string {
	return fmt.Sprintf("%s%s-%s-%d", RequestIDPrefix, runID, step, attempt)
}

// nonce is a request id for a write that belongs to no step run, such as the
// pair of label writes a claim brackets itself with.
func nonce(what string) string {
	buf := make([]byte, 8)
	// crypto/rand.Read never returns an error; the signature keeps one for
	// compatibility. See the package documentation.
	_, _ = rand.Read(buf)
	return RequestIDPrefix + what + "-" + hex.EncodeToString(buf)
}
