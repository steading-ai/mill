//go:build darwin

package interceptor

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredentials asks the kernel who is on the other end of conn. Darwin
// reports a unix socket's peer through LOCAL_PEERCRED, which carries the
// uid and groups, and its pid through LOCAL_PEERPID.
func peerCredentials(conn *net.UnixConn) (PeerCredentials, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCredentials{}, fmt.Errorf("interceptor: peer credentials: %w", err)
	}
	var (
		xucred  *unix.Xucred
		pid     int
		sockErr error
	)
	if err := raw.Control(func(fd uintptr) {
		xucred, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if sockErr != nil {
			return
		}
		pid, sockErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return PeerCredentials{}, fmt.Errorf("interceptor: peer credentials: %w", err)
	}
	if sockErr != nil {
		return PeerCredentials{}, fmt.Errorf("interceptor: LOCAL_PEERCRED: %w", sockErr)
	}
	gid := 0
	if xucred.Ngroups > 0 {
		gid = int(xucred.Groups[0])
	}
	return PeerCredentials{UID: int(xucred.Uid), GID: gid, PID: pid}, nil
}
