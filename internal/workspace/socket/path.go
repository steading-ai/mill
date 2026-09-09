package socket

import (
	"fmt"
	"syscall"
)

// A unix socket is bound by copying its path into the kernel's sockaddr_un,
// whose sun_path is a fixed-size array — 108 bytes on Linux, 104 on Darwin —
// and must hold the terminator too. A path one byte over that fails as
// `bind: invalid argument`, which says nothing about the path, the length or
// the limit, and which the mill hits in two places: the service's own socket
// under a long --socket, and a run's workspace socket under a long
// --workspace-dir. The second fails after the node has been claimed, so the
// path is checked before anything is claimed or bound.

// MaxPathLen is the longest path this kernel will bind a unix socket at. It
// is sun_path's size less the byte the terminator takes.
const MaxPathLen = len(syscall.RawSockaddrUnix{}.Path) - 1

// PathTooLongError is a socket path the kernel cannot hold. It names the
// path, its length and the limit, because the fix is to shorten the path and
// nothing else says by how much.
type PathTooLongError struct {
	// Path is the path that would have been bound.
	Path string
	// Limit is [MaxPathLen], carried so a reader of the error needs nothing
	// else.
	Limit int
}

func (e *PathTooLongError) Error() string {
	return fmt.Sprintf("socket: %s is %d bytes long, and a unix socket path can be at most %d "+
		"(the kernel's sun_path holds %d bytes including its terminator): use a shorter path",
		e.Path, len(e.Path), e.Limit, e.Limit+1)
}

// CheckPath reports whether path could be bound as a unix socket, as a
// *[PathTooLongError] when it could not.
func CheckPath(path string) error {
	if len(path) > MaxPathLen {
		return &PathTooLongError{Path: path, Limit: MaxPathLen}
	}
	return nil
}
