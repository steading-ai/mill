package dagger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
)

// Claimed is a node and the lease fencing it.
type Claimed struct {
	// Node is the claimed node, in full.
	Node *skiftv1.Node
	// Lease is the claim. Its id is the fencing token every later write to
	// the node carries.
	Lease *skiftv1.Lease
	// Project is the project the node was claimed in, as the claim
	// response carried it: where the work delivers, read without a second
	// call. Nil from a dagger older than the field, which [Client.GetProject]
	// answers for.
	Project *skiftv1.Project
}

// Claim takes a lease on one named node.
//
// Dagger has no per-node claim: `ClaimNext` is the whole of the lease side's
// admission, and it hands back whatever the ready set puts first. Naming the
// node is therefore done the way dagger's own harness does it — a label only
// this claim knows, written before the claim and removed after it, with the
// claim filtered to that label. The two label writes are ordinary patches
// under `mill-` request ids, and a crash between them leaves a label a later
// claim overwrites rather than a node in a wrong state.
//
// A node that is not ready is explained and refused. `mill run` does not
// wait: the reason a node cannot be claimed is a thing to tell the operator,
// not a thing to block on.
func (c *Client) Claim(ctx context.Context, ref string) (*Claimed, error) {
	node, err := c.Node(ctx, ref)
	if err != nil {
		return nil, err
	}
	if node.GetAssigneeKind() == skiftv1.AssigneeKind_ASSIGNEE_KIND_USER {
		return nil, &UserNodeError{Ref: ref, Verb: "claim"}
	}
	if !node.GetReady() {
		reasons, err := c.Explain(ctx, ref)
		if err != nil {
			return nil, err
		}
		return nil, &NotReadyError{Ref: ref, Reasons: reasons}
	}

	label := nonce("claim")
	restore := slices.Clone(node.GetLabels())
	if err := c.setLabels(ctx, "", nonce("label"), ref, append(slices.Clone(restore), label)); err != nil {
		return nil, err
	}

	claimed, claimErr := c.claimLabelled(ctx, label, node.GetId())
	leaseID := ""
	if claimed != nil {
		leaseID = claimed.Lease.GetId()
	}
	// The label is mill's bookkeeping and never the node's, so it comes off
	// whether or not the claim succeeded. A failure to tidy up is reported
	// only when there is nothing worse to report.
	if err := c.setLabels(ctx, leaseID, nonce("label"), ref, restore); err != nil && claimErr == nil {
		return nil, err
	}
	if claimErr != nil {
		return nil, claimErr
	}
	return claimed, nil
}

// claimLabelled asks for the next ready node carrying label and insists it is
// the node the caller meant.
func (c *Client) claimLabelled(ctx context.Context, label, nodeID string) (*Claimed, error) {
	res, err := c.work.ClaimNext(ctx, connect.NewRequest(&skiftv1.ClaimNextRequest{
		RequestId:  nonce("claim"),
		Filter:     &skiftv1.NodeFilter{Projects: []string{c.project}, Labels: []string{label}},
		Ttl:        durationOrNil(c.options.ttl),
		Harness:    HarnessName,
		AgentRef:   c.options.agentRef,
		SessionRef: c.options.sessionRef,
	}))
	if err != nil {
		return nil, wrap("dagger: claim", err)
	}
	node, lease := res.Msg.GetNode(), res.Msg.GetLease()
	if node == nil || lease == nil {
		return nil, &NotReadyError{Ref: nodeID}
	}
	if node.GetId() != nodeID {
		// A label only this claim knows cannot match another node, so this
		// is a bug rather than a race. Give the lease back rather than hold
		// one on work nobody asked for.
		_ = c.Release(ctx, lease.GetId(), skiftv1.ReleaseReason_RELEASE_REASON_PREEMPTED, "mill: not the node asked for")
		return nil, fmt.Errorf("dagger: claim returned %s, not the node asked for", node.GetReference())
	}
	return &Claimed{Node: node, Lease: lease, Project: res.Msg.GetProject()}, nil
}

// Heartbeat renews a lease. A refusal is [ErrFenced]: the lease expired and
// somebody else holds the node, which is the signal to stop.
func (c *Client) Heartbeat(ctx context.Context, leaseID string, ttl time.Duration) (*skiftv1.Lease, error) {
	res, err := c.work.Heartbeat(ctx, connect.NewRequest(&skiftv1.HeartbeatRequest{
		RequestId: nonce("heartbeat"),
		LeaseId:   leaseID,
		Ttl:       durationOrNil(ttl),
	}))
	if err != nil {
		return nil, wrap("dagger: heartbeat", err)
	}
	return res.Msg.GetLease(), nil
}

// Release ends a lease with a reason. There is no Release on the done path:
// the completing patch releases the lease as part of itself.
func (c *Client) Release(ctx context.Context, leaseID string, reason skiftv1.ReleaseReason, detail string) error {
	_, err := c.work.Release(ctx, connect.NewRequest(&skiftv1.ReleaseRequest{
		RequestId: nonce("release"),
		LeaseId:   leaseID,
		Reason:    reason,
		Detail:    detail,
	}))
	return wrap("dagger: release", err)
}

// TokenCounts is one model's tokens for one step run.
type TokenCounts struct {
	// InputTokens were sent to the model.
	InputTokens int64
	// OutputTokens were produced by it.
	OutputTokens int64
	// CacheReadTokens were served from its prompt cache.
	CacheReadTokens int64
	// CacheWriteTokens were written to it.
	CacheWriteTokens int64
}

// ReportUsage records what one step run cost, attributed to the lease it ran
// under. Dagger sums records per attempt, and each report renews the lease,
// so posting per step is both the contract's shape and a second heartbeat.
func (c *Client) ReportUsage(ctx context.Context, leaseID, ref, model string, counts TokenCounts) error {
	_, err := c.work.ReportUsage(ctx, connect.NewRequest(&skiftv1.ReportUsageRequest{
		RequestId:        nonce("usage"),
		Project:          c.project,
		LeaseId:          leaseID,
		Node:             ref,
		Model:            model,
		InputTokens:      counts.InputTokens,
		OutputTokens:     counts.OutputTokens,
		CacheReadTokens:  counts.CacheReadTokens,
		CacheWriteTokens: counts.CacheWriteTokens,
	}))
	return wrap("dagger: report usage", err)
}

// durationOrNil leaves a zero duration unset, so the server applies its own
// default rather than being asked for none.
func durationOrNil(d time.Duration) *durationpb.Duration {
	if d <= 0 {
		return nil
	}
	return durationpb.New(d)
}

// Ready is the ready set of projects, as dagger reports it to this caller. An
// empty projects means every project this caller may read. It is a read:
// nothing is claimed and nothing is written.
//
// `mill doctor` uses it as its liveness probe, because dagger resolves a
// principal from the transport and refuses a uid it does not know, so an
// answer proves both that the service is there and that this account reaches
// it as a principal.
func (c *Client) Ready(ctx context.Context, projects []string, kind skiftv1.AssigneeKind) ([]*skiftv1.Node, error) {
	res, err := c.work.Ready(ctx, connect.NewRequest(&skiftv1.ReadyRequest{
		Filter: &skiftv1.NodeFilter{Projects: projects, AssigneeKind: kind},
		View:   skiftv1.View_VIEW_REFS,
	}))
	if err != nil {
		return nil, fmt.Errorf("dagger: ready: %w", err)
	}
	return res.Msg.GetNodes(), nil
}

// claimChunk bounds one ClaimNext request inside a longer wait. Dagger
// clamps a wait to five minutes and polls its ready set at most a second
// apart, so a long wait is many short requests rather than one held
// connection — the same shape `dg claim --wait` takes.
const claimChunk = time.Minute

// ClaimNext is the worker's claim: the next ready node of projects matching
// labels, already leased. An empty projects means every project this caller
// may read. A nil node with a nil error is the wait elapsing with nothing to
// claim, which is the ordinary answer on an idle graph and not a failure.
//
// The filters are the whole of the selection a worker makes
// (docs/architecture.md §"Request path", step 1). `assignee_kind: agent` is
// not one of them in the configurable sense: it is the seam's refusal to
// claim a node only a person may complete, which includes every gate mill
// itself files (docs/domain-model.md §Principal). Dagger reads it as "agent
// or unassigned", so an ordinary node is still claimable.
//
// The request id is held across the chunks of one wait: an empty answer
// writes nothing, so the key is unused until a claim lands, and a retry
// after a dropped connection then returns the same node rather than a
// second one.
func (c *Client) ClaimNext(ctx context.Context, projects, labels []string, wait time.Duration) (*Claimed, error) {
	requestID := nonce("claim")
	deadline := time.Now().Add(wait)
	for {
		res, err := c.work.ClaimNext(ctx, connect.NewRequest(&skiftv1.ClaimNextRequest{
			RequestId: requestID,
			Filter: &skiftv1.NodeFilter{
				Projects:     projects,
				AssigneeKind: skiftv1.AssigneeKind_ASSIGNEE_KIND_AGENT,
				Labels:       labels,
			},
			Wait:       durationpb.New(max(min(claimChunk, time.Until(deadline)), 0)),
			Ttl:        durationOrNil(c.options.ttl),
			Harness:    HarnessName,
			AgentRef:   c.options.agentRef,
			SessionRef: c.options.sessionRef,
		}))
		if err != nil {
			return nil, wrap("dagger: claim next", err)
		}
		if node, lease := res.Msg.GetNode(), res.Msg.GetLease(); node != nil && lease != nil {
			return &Claimed{Node: node, Lease: lease, Project: res.Msg.GetProject()}, nil
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}
