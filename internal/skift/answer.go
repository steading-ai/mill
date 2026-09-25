package skift

import (
	"context"

	"connectrpc.com/connect"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
)

// AnswerGate is the one write in this package that is not mill's. It is the
// person at the terminal answering a gate `mill run` filed a moment ago,
// performed on their behalf over the connection their uid already owns.
//
// Everything else here refuses to complete, drop, reclassify or unlink a
// user-assigned node, because mill must not dispose of the gates it files
// (docs/domain-model.md §Principal). That refusal is mill's discipline and
// not skift's rule: skift scopes the filer restrictions to non-user
// principals, and mill runs as the operator's user principal, so a person
// answering their own gate through this process is exactly the case skift's
// §Gates allows. The distinction the ledger has to keep is therefore *who
// acted*, which is why this carries no lease and a request id that does not
// begin with mill's prefix: nothing here is attributable to the mill.
//
// It is called from one place — `mill run`'s terminal prompt — and never
// from the run loop.
func (c *Client) AnswerGate(ctx context.Context, requestID, ref string, approve bool, result *skiftv1.Result) error {
	// A denial is a drop with no cascade: the gate goes and the node it
	// blocked stays, so the run can complete that node with the gate's
	// deny_result and say why.
	op := &skiftv1.PatchOp{Op: &skiftv1.PatchOp_DropNode{DropNode: &skiftv1.DropNodeOp{
		Node:   ref,
		Reason: skiftv1.DropReason_DROP_REASON_OTHER,
		Detail: result.GetBody(),
	}}}
	if approve {
		op = &skiftv1.PatchOp{Op: &skiftv1.PatchOp_CompleteNode{CompleteNode: &skiftv1.CompleteNodeOp{
			Node:   ref,
			Result: result,
		}}}
	}
	_, err := c.graph.ApplyPatch(ctx, connect.NewRequest(&skiftv1.ApplyPatchRequest{
		RequestId: requestID,
		Project:   c.project,
		Ops:       []*skiftv1.PatchOp{op},
		View:      skiftv1.View_VIEW_REFS,
	}))
	return wrap("skift: answer gate "+ref, err)
}

// UserRequestIDPrefix begins a request id for a write a person made through
// mill's process. It is deliberately not [RequestIDPrefix]: skift's event
// log should not say the mill did what a human did.
const UserRequestIDPrefix = "user-"
