//go:build !linux && !darwin

package interceptor

import (
	"errors"
	"net"
)

// peerCredentials is unavailable off Linux and Darwin, where the kernel
// has no option this package reads a peer by. Every caller is then
// unauthenticated, which is the safe direction to fail in.
func peerCredentials(*net.UnixConn) (PeerCredentials, error) {
	return PeerCredentials{}, errors.New("interceptor: peer credentials are not available on this platform")
}
