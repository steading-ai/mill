package dagger

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
)

// ErrUserNode is the refusal docs/domain-model.md §Principal makes mill's
// own: a node whose assignee_kind is `user` is a gate, and mill never
// claims, completes, drops, reclassifies, or unlinks one — including a gate
// it filed itself. Dagger permits all five to the operator's principal mill
// runs as, so nothing but this refuses them.
var ErrUserNode = errors.New("dagger: the node is assigned to a user; mill does not act on gates")

// ErrFenced is a dagger write refused because the lease presented is not the
// node's current one. It is the normal path for a stale worker, not an error
// to retry: the run is abandoned locally and the new holder owns it.
var ErrFenced = errors.New("dagger: the lease is not the node's current one")

// UserNodeError names the node and the verb that was refused.
type UserNodeError struct {
	// Ref is the node reference.
	Ref string
	// Verb is what was refused: claim, complete, drop, reclassify, unlink.
	Verb string
}

func (e *UserNodeError) Error() string {
	return fmt.Sprintf("dagger: refusing to %s %s: it is assigned to a user", e.Verb, e.Ref)
}

// Unwrap makes every refusal match [ErrUserNode].
func (e *UserNodeError) Unwrap() error { return ErrUserNode }

// NotReadyError is a claim on a node dagger will not hand over, carrying the
// reasons `explain` gave. The work order's rule is to say why and stop
// rather than wait.
type NotReadyError struct {
	// Ref is the node reference.
	Ref string
	// Reasons is one sentence per condition of readiness that does not hold.
	Reasons []string
}

func (e *NotReadyError) Error() string {
	if len(e.Reasons) == 0 {
		return fmt.Sprintf("dagger: %s is not claimable", e.Ref)
	}
	return fmt.Sprintf("dagger: %s is not ready: %s", e.Ref, strings.Join(e.Reasons, "; "))
}

// fenced reports whether a Connect error is dagger refusing a write because
// the lease is stale or expired. Dagger answers a fenced write with
// FAILED_PRECONDITION; the message is what distinguishes it from the other
// preconditions, and both halves are checked so a precondition of a
// different kind is not mistaken for a fence.
func fenced(err error) bool {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return false
	}
	if connectErr.Code() != connect.CodeFailedPrecondition && connectErr.Code() != connect.CodeNotFound {
		return false
	}
	message := strings.ToLower(connectErr.Message())
	for _, mark := range []string{"lease", "fence", "expired", "stale"} {
		if strings.Contains(message, mark) {
			return true
		}
	}
	return false
}

// wrap turns a Connect failure into an error the run loop can classify,
// keeping the original for anything that wants to read the code.
func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	if fenced(err) {
		return fmt.Errorf("%s: %w: %w", what, ErrFenced, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}
