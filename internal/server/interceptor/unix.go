package interceptor

import (
	"context"
	"net"
)

// PeerCredentials are what the kernel reports about the process at the
// other end of a unix socket.
type PeerCredentials struct {
	UID int
	GID int
	PID int
}

type peerCredentialsKey struct{}

// UnixConnContext is an http.Server ConnContext. When conn is a unix socket
// connection it reads the peer's credentials with SO_PEERCRED and stores
// them on the context every request on that connection inherits. Any other
// connection passes through untouched, and the role interceptor refuses it
// as unauthenticated.
func UnixConnContext(ctx context.Context, conn net.Conn) context.Context {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return ctx
	}
	creds, err := peerCredentials(uc)
	if err != nil {
		return ctx
	}
	return WithPeerCredentials(ctx, creds)
}

// WithPeerCredentials returns a context carrying creds as if they had been
// read from the connection. Specs use it to stand in for a caller under
// another uid.
func WithPeerCredentials(ctx context.Context, creds PeerCredentials) context.Context {
	return context.WithValue(ctx, peerCredentialsKey{}, creds)
}

// PeerCredentialsFromContext returns the credentials [UnixConnContext]
// attached, and false when the request did not arrive over a unix socket.
func PeerCredentialsFromContext(ctx context.Context) (PeerCredentials, bool) {
	creds, ok := ctx.Value(peerCredentialsKey{}).(PeerCredentials)
	return creds, ok
}
