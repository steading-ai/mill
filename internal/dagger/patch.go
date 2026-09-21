package dagger

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
)

// ApplyPatch is the one door to every graph mutation mill makes. Before the
// patch is sent, every operation that would claim, complete, drop,
// reclassify, or unlink a node is checked against that node's assignee
// kind, and the whole patch is refused if any of them touches a user's node
// (docs/domain-model.md §Principal). The check is a read per named node,
// which is the price of the guarantee being in mill's code rather than in
// dagger's policy — dagger permits all five to the principal mill runs as.
func (c *Client) ApplyPatch(ctx context.Context, requestID, leaseID string, ops ...*daggerv1.PatchOp) (*daggerv1.ApplyPatchResponse, error) {
	if err := c.refuseGateWrites(ctx, ops); err != nil {
		return nil, err
	}
	res, err := c.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: requestID,
		Project:   c.project,
		LeaseId:   leaseID,
		Ops:       ops,
		View:      daggerv1.View_VIEW_REFS,
	}))
	if err != nil {
		return nil, wrap("dagger: apply patch", err)
	}
	return res.Msg, nil
}

// refuseGateWrites is the enforcement of mill's five refusals. Each verb is
// named so the error says which one was attempted.
func (c *Client) refuseGateWrites(ctx context.Context, ops []*daggerv1.PatchOp) error {
	for _, op := range ops {
		var ref, verb string
		switch o := op.GetOp().(type) {
		case *daggerv1.PatchOp_CompleteNode:
			ref, verb = o.CompleteNode.GetNode(), "complete"
		case *daggerv1.PatchOp_DropNode:
			ref, verb = o.DropNode.GetNode(), "drop"
		case *daggerv1.PatchOp_UpdateNode:
			// Reclassifying is changing who may finish a node. Every other
			// field of an update leaves the gate a gate.
			if o.UpdateNode.AssigneeKind == nil && o.UpdateNode.Assignee == nil {
				continue
			}
			ref, verb = o.UpdateNode.GetNode(), "reclassify"
		case *daggerv1.PatchOp_RemoveEdge:
			// Unlinking a gate is removing the edge by which it blocks. Both
			// ends are checked: the gate is the `from` of a blocks edge, and
			// a gate that contains something is the `to`.
			for _, end := range []string{o.RemoveEdge.GetFrom(), o.RemoveEdge.GetTo()} {
				if err := c.refuseIfUser(ctx, end, "unlink"); err != nil {
					return err
				}
			}
			continue
		default:
			continue
		}
		if err := c.refuseIfUser(ctx, ref, verb); err != nil {
			return err
		}
	}
	return nil
}

// refuseIfUser reads a node and refuses the verb when a user owns it. A
// reference the patch itself creates — an alias — names no node yet and
// cannot be a gate mill did not file, so a failed lookup is not a refusal.
func (c *Client) refuseIfUser(ctx context.Context, ref, verb string) error {
	if ref == "" {
		return nil
	}
	node, err := c.Node(ctx, ref)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable reference is an alias, not a gate
	}
	if node.GetAssigneeKind() == daggerv1.AssigneeKind_ASSIGNEE_KIND_USER {
		return &UserNodeError{Ref: ref, Verb: verb}
	}
	return nil
}

// Complete finishes a node with a result and its artifacts. Completing is
// not an RPC: it is an ApplyPatch carrying a complete_node operation, and
// dagger releases the lease inside that same patch — which is why the final
// step's usage is posted before this, why there is no Release on the done
// path, and why store.Done comes after it (docs/ledger.md §Resume).
func (c *Client) Complete(ctx context.Context, requestID, leaseID, ref string, result *daggerv1.Result) error {
	_, err := c.ApplyPatch(ctx, requestID, leaseID, &daggerv1.PatchOp{
		Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
			Node:   ref,
			Result: result,
		}},
	})
	return err
}

// Comment adds a comment to a node: the last findings on a park.
func (c *Client) Comment(ctx context.Context, requestID, leaseID, ref, body string) error {
	_, err := c.ApplyPatch(ctx, requestID, leaseID, &daggerv1.PatchOp{
		Op: &daggerv1.PatchOp_AddComment{AddComment: &daggerv1.AddCommentOp{
			Node: ref,
			Body: body,
		}},
	})
	return err
}

// Gate is what a gate step or the circuit breaker asks a human.
type Gate struct {
	// Title is the gate node's title.
	Title string
	// Body is the ask.
	Body string
	// Blocks is the node the gate holds up: the run's own node.
	Blocks string
	// Labels are carried onto the gate node.
	Labels []string
}

// gateAlias names the created gate for the edge in the same patch.
const gateAlias = "gate"

// CreateGate files a human decision the only way dagger allows: a node with
// assignee_kind `user`, a child of the run's node, with a blocks edge into
// it. One patch, so the gate and the edge that makes it block arrive
// together; the store records the id it returns before the lease is
// released (docs/architecture.md §"Gate steps").
func (c *Client) CreateGate(ctx context.Context, requestID, leaseID string, gate Gate) (*daggerv1.Node, error) {
	res, err := c.ApplyPatch(ctx, requestID, leaseID,
		&daggerv1.PatchOp{Op: &daggerv1.PatchOp_CreateNode{CreateNode: &daggerv1.CreateNodeOp{
			Alias:        gateAlias,
			Parent:       gate.Blocks,
			Kind:         "chore",
			Title:        gate.Title,
			Body:         gate.Body,
			AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER,
			Labels:       gate.Labels,
		}}},
		&daggerv1.PatchOp{Op: &daggerv1.PatchOp_AddEdge{AddEdge: &daggerv1.AddEdgeOp{
			From: gateAlias,
			To:   gate.Blocks,
			Kind: daggerv1.EdgeKind_EDGE_KIND_BLOCKS,
		}}},
	)
	if err != nil {
		return nil, err
	}
	ref, ok := res.GetAliases()[gateAlias]
	if !ok {
		return nil, fmt.Errorf("dagger: the patch that filed the gate returned no %q alias", gateAlias)
	}
	return c.Node(ctx, ref)
}

// setLabels replaces a node's label set. It is the half of [Client.Claim]
// that names the node the claim filter will match.
func (c *Client) setLabels(ctx context.Context, leaseID, requestID, ref string, labels []string) error {
	_, err := c.ApplyPatch(ctx, requestID, leaseID, &daggerv1.PatchOp{
		Op: &daggerv1.PatchOp_UpdateNode{UpdateNode: &daggerv1.UpdateNodeOp{
			Node:   ref,
			Labels: &daggerv1.Labels{Values: labels},
		}},
	})
	return err
}

// AttachText attaches an artifact carrying its own content to a node: a
// plan, a verdict, a step's summary. Every artifact mill attaches carries
// the run id in its metadata, which is how dagger's side of a run joins to
// the ledger's.
func (c *Client) AttachText(ctx context.Context, requestID, leaseID, ref, name, description, body string, meta map[string]any) error {
	value, err := structpb.NewStruct(meta)
	if err != nil {
		return fmt.Errorf("dagger: artifact %s metadata: %w", name, err)
	}
	_, err = c.ApplyPatch(ctx, requestID, leaseID, &daggerv1.PatchOp{
		Op: &daggerv1.PatchOp_AttachArtifact{AttachArtifact: &daggerv1.AttachArtifactOp{
			Node: ref,
			Artifact: &daggerv1.Artifact{
				Name:        name,
				Description: description,
				Parts: []*daggerv1.Part{{
					Content: &daggerv1.Part_Text{Text: &daggerv1.TextPart{
						Text:      body,
						MediaType: "text/markdown",
					}},
				}},
				Metadata: value,
			},
		}},
	})
	return err
}

// Artifacts are what a node carries. The `artifact` verifier reads them:
// evidence of that kind is a digest dagger accepted, so dagger is asked.
func (c *Client) Artifacts(ctx context.Context, ref string) ([]*daggerv1.Artifact, error) {
	res, err := c.graph.GetNode(ctx, connect.NewRequest(&daggerv1.GetNodeRequest{
		Project: c.project,
		Node:    ref,
		View:    daggerv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, fmt.Errorf("dagger: get artifacts of %s: %w", ref, err)
	}
	return res.Msg.GetArtifacts(), nil
}

// GateLabels are the labels mill puts on every gate it files: one saying
// mill filed it, one naming the run, one naming the step.
//
// They are how a gate is found again. Dagger resolves references — `mill#7`
// — and never ids, so the id the ledger holds is not something dagger can be
// asked for; a run's own gates are found among its node's children by these
// labels and matched on the id there. They are also how a worker finds a
// gate it filed before it crashed.
func GateLabels(runID, step string) []string {
	return []string{GateLabel, RunLabel(runID), StepLabel(step)}
}

// RunLabel names the run a gate belongs to.
func RunLabel(runID string) string { return "mill-run-" + runID }

// StepLabel names the step a gate belongs to.
func StepLabel(step string) string { return "mill-step-" + step }

// GateLabel marks every node mill files as a gate.
const GateLabel = "mill-gate"

// Gates are the user-assigned children of a node carrying every one of the
// given labels: the gates mill filed on it, and nothing a human filed for
// their own reasons.
//
// It exists for the one window the write order of docs/ledger.md §Resume
// cannot close. Filing a gate is dagger first, store second: a crash between
// them leaves a gate dagger has and the store does not, and a rerun that
// trusted the store alone would file a second one for the same step.
// Dagger's graph is the source of truth for that single check, so the worker
// asks it before it files (docs/domain-model.md §Gate, "A gate is filed
// once").
//
// Status is deliberately not filtered. The gate is usually still open — that
// is what the crash left behind — but the node it blocks is not claimable
// until somebody answers it, so the rerun that finds it is often the one
// that follows the answer, and a search for open gates alone would miss the
// gate precisely when it is about to file a second.
func (c *Client) Gates(ctx context.Context, parent string, labels ...string) ([]*daggerv1.Node, error) {
	res, err := c.graph.ListNodes(ctx, connect.NewRequest(&daggerv1.ListNodesRequest{
		Filter: &daggerv1.NodeFilter{
			Projects:     []string{c.project},
			Parent:       parent,
			AssigneeKind: daggerv1.AssigneeKind_ASSIGNEE_KIND_USER,
			Labels:       labels,
		},
		View: daggerv1.View_VIEW_FULL,
	}))
	if err != nil {
		return nil, wrap("dagger: list the gates on "+parent, err)
	}
	return res.Msg.GetNodes(), nil
}
