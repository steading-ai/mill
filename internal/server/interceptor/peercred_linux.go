//go:build linux

package interceptor

import (
	"fmt"
	"net"
	"syscall"
)

// peerCredentials asks the kernel who is on the other end of conn.
func peerCredentials(conn *net.UnixConn) (PeerCredentials, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCredentials{}, fmt.Errorf("interceptor: peer credentials: %w", err)
	}
	var (
		ucred   *syscall.Ucred
		sockErr error
	)
	if err := raw.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return PeerCredentials{}, fmt.Errorf("interceptor: peer credentials: %w", err)
	}
	if sockErr != nil {
		return PeerCredentials{}, fmt.Errorf("interceptor: SO_PEERCRED: %w", sockErr)
	}
	return PeerCredentials{UID: int(ucred.Uid), GID: int(ucred.Gid), PID: int(ucred.Pid)}, nil
}
