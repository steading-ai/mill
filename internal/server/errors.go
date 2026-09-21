package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/ivy/mill/internal/domain"
)

// Every error a handler returns is a Connect error whose code says what kind
// of failure it was, so a worker can branch on the code and a person can
// read the message. The domain reports failures as typed errors and
// sentinels; this is the one place they become codes.

// connectError maps err to a Connect error. A Connect error passes through
// unchanged. Anything the domain did not type is internal.
func connectError(err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	return connect.NewError(codeOf(err), err)
}

// codeOf is the Connect code for one domain error. Typed errors are matched
// with errors.As and sentinels with errors.Is, so wrapping never hides the
// code: an error naming the run still reports its cause's code, and the
// cause's message — which is where the two lease ids of a fenced write, or
// the two states of a refused transition, are named.
func codeOf(err error) connect.Code {
	var (
		transition     *domain.RunTransitionError
		stepTransition *domain.StepRunTransitionError
		ended          *domain.RunEndedError
		evidence       *domain.EvidenceError
		mcpCollision   *domain.McpCollisionError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded

	// The request itself does not hold together: evidence whose value is
	// not the shape its kind defines, a workflow whose mcp-servers collides
	// with the host's own, or a call with nothing to act on.
	case errors.As(err, &evidence), errors.As(err, &mcpCollision), errors.Is(err, errNoTarget):
		return connect.CodeInvalidArgument

	// A row the request named does not exist.
	case errors.Is(err, domain.ErrNotFound):
		return connect.CodeNotFound

	// The ledger is not in the state the request needs: a lease that is no
	// longer the run's, a transition the diagram does not draw, an attempt
	// that has already ended, a run that has. Nothing about who asked is at
	// issue, only what the record looks like now.
	case errors.Is(err, domain.ErrFenced),
		errors.As(err, &transition),
		errors.As(err, &stepTransition),
		errors.As(err, &ended),
		errors.Is(err, domain.ErrNotClaimable),
		errors.Is(err, domain.ErrNotRunning),
		errors.Is(err, domain.ErrUnknownStep),
		errors.Is(err, domain.ErrNoWorkflow),
		errors.Is(err, domain.ErrLeased),
		errors.Is(err, domain.ErrNotAttempted):
		return connect.CodeFailedPrecondition

	default:
		return connect.CodeInternal
	}
}

// errNoTarget is a request that named nothing to act on, such as an Inspect
// with neither a run id nor a node ref.
var errNoTarget = errors.New("the request names nothing to act on")
