package dagger

import (
	"context"

	"connectrpc.com/connect"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
)

// AnswerGate is the one write in this package that is not mill's. It is the
// person at the terminal answering a gate `mill run` filed a moment ago,
// performed on their behalf over the connection their uid already owns.
//
// Everything else here refuses to complete, drop, reclassify or unlink a
// user-assigned node, because mill must not dispose of the gates it files
// (docs/domain-model.md §Principal). That refusal is mill's discipline and
// not dagger's rule: dagger scopes the filer restrictions to non-user
// principals, and mill runs as the operator's user principal, so a person
// answering their own gate through this process is exactly the case dagger's
// §Gates allows. The distinction the ledger has to keep is therefore *who
// acted*, which is why this carries no lease and a request id that does not
// begin with mill's prefix: nothing here is attributable to the mill.
//
// It is called from one place — `mill run`'s terminal prompt — and never
// from the run loop.
func (c *Client) AnswerGate(ctx context.Context, requestID, ref string, approve bool, result *daggerv1.Result) error {
	// A denial is a drop with no cascade: the gate goes and the node it
	// blocked stays, so the run can complete that node with the gate's
	// deny_result and say why.
	op := &daggerv1.PatchOp{Op: &daggerv1.PatchOp_DropNode{DropNode: &daggerv1.DropNodeOp{
		Node:   ref,
		Reason: daggerv1.DropReason_DROP_REASON_OTHER,
		Detail: result.GetBody(),
	}}}
	if approve {
		op = &daggerv1.PatchOp{Op: &daggerv1.PatchOp_CompleteNode{CompleteNode: &daggerv1.CompleteNodeOp{
			Node:   ref,
			Result: result,
		}}}
	}
	_, err := c.graph.ApplyPatch(ctx, connect.NewRequest(&daggerv1.ApplyPatchRequest{
		RequestId: requestID,
		Project:   c.project,
		Ops:       []*daggerv1.PatchOp{op},
		View:      daggerv1.View_VIEW_REFS,
	}))
	return wrap("dagger: answer gate "+ref, err)
}

// UserRequestIDPrefix begins a request id for a write a person made through
// mill's process. It is deliberately not [RequestIDPrefix]: dagger's event
// log should not say the mill did what a human did.
const UserRequestIDPrefix = "user-"
