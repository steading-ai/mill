//go:build !linux

package interceptor

import (
	"errors"
	"net"
)

// peerCredentials is unavailable off Linux, where SO_PEERCRED is not the
// way the kernel reports a socket's peer. Every caller is then
// unauthenticated, which is the safe direction to fail in.
func peerCredentials(*net.UnixConn) (PeerCredentials, error) {
	return PeerCredentials{}, errors.New("interceptor: peer credentials are not available on this platform")
}
