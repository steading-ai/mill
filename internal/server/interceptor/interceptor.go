// Package interceptor holds what sits between the transport and the store's
// handlers: identity, and what the identity may call.
//
// Identity is the peer uid. The store's socket is reached by unix
// connections alone, and SO_PEERCRED reports the account at the other end,
// so a caller is a uid and nothing else — there is no token scheme and
// nothing in a request body says who sent it. A uid is mapped by
// configuration to a role, and a role to what it may call. See the Principal
// section of docs/domain-model.md and the "Identity and credentials" section
// of docs/architecture.md.
//
// Mill and the operator share a uid at this maturity, so the mill's own uid
// is the one that does everything and the role map exists for other uids:
// the zone agents, which read and operate but never hold a lease.
package interceptor

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

// Role is what a uid may do on the store's socket.
type Role string

// The roles. Nothing else is a role: a uid the map does not name reaches
// nothing.
const (
	// RoleService is the account the service itself runs as. It does
	// everything, because the worker face of the same binary is the only
	// caller that writes a ledger.
	RoleService Role = "service"
	// RoleOperator reads and operates: cancel, restart, workflow push.
	RoleOperator Role = "operator"
	// RoleReader reads.
	RoleReader Role = "reader"
)

// Permission is how much authority a call needs.
type Permission int

// The three levels, in increasing order of authority. A role holds one and
// admits every call needing that much or less.
const (
	// PermissionRead sees the ledger and changes nothing.
	PermissionRead Permission = iota + 1
	// PermissionOperate asks the worker for something, or stores a
	// definition: the verbs a human or an agent drives.
	PermissionOperate
	// PermissionWrite writes the record of a run. The worker's own calls,
	// which no other uid makes.
	PermissionWrite
)

// grants is what each role holds.
var grants = map[Role]Permission{
	RoleService:  PermissionWrite,
	RoleOperator: PermissionOperate,
	RoleReader:   PermissionRead,
}

// Requirement says how much authority a procedure needs. The server owns
// the table; the interceptor knows only how to compare.
type Requirement func(procedure string) Permission

// Caller is the resolved actor of a request: a uid and the role
// configuration gave it.
type Caller struct {
	UID  int
	Role Role
}

// String is how the caller appears in the ledger's events and in a refusal.
func (c Caller) String() string { return fmt.Sprintf("uid:%d(%s)", c.UID, c.Role) }

type callerKey struct{}

// WithCaller returns a context attributing requests to c. The interceptor
// calls it; handler specs call it to stand in for a transport.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFromContext returns the caller the interceptor resolved, and false
// when the request was not attributed. A handler behind the interceptor
// always finds one.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// roleInterceptor resolves the peer uid and checks what its role may call.
type roleInterceptor struct {
	roles    RoleMap
	required Requirement
}

// NewRoles returns the interceptor in front of every handler: it maps the
// connection's peer uid to a role and refuses the call when the role does
// not carry it. A request that arrived over something other than a unix
// socket, or under a uid the map does not name, is unauthenticated — the
// service invents no caller and reads none from a request body.
func NewRoles(roles RoleMap, required Requirement) connect.Interceptor {
	return &roleInterceptor{roles: roles, required: required}
}

// admit resolves the caller and checks the procedure against its role.
func (i *roleInterceptor) admit(ctx context.Context, procedure string) (context.Context, error) {
	creds, ok := PeerCredentialsFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("interceptor: no peer credentials: the store's API is reached over its unix socket"))
	}
	role, ok := i.roles.Role(creds.UID)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("interceptor: uid %d holds no role on this socket", creds.UID))
	}
	caller := Caller{UID: creds.UID, Role: role}
	if grants[role] < i.required(procedure) {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("interceptor: role %s may not call %s", role, procedure))
	}
	return WithCaller(ctx, caller), nil
}

// WrapUnary implements [connect.Interceptor].
func (i *roleInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.admit(ctx, req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient implements [connect.Interceptor]. Admission is a
// server concern, so the client side passes through.
func (i *roleInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements [connect.Interceptor].
func (i *roleInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.admit(ctx, conn.Spec().Procedure)
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
