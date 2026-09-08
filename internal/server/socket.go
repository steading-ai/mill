package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// listenUnix binds cfg.SocketPath and sets its mode.
//
// The socket is created with the process umask, then tightened. The parent
// directory is what guards the gap: under systemd it is a RuntimeDirectory
// with its own mode, and a caller that can traverse it could reach the
// socket in any case.
func listenUnix(cfg Config) (net.Listener, error) {
	path := cfg.SocketPath
	if path == "" {
		return nil, errors.New("server: socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("server: create socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("server: listen: %w", err)
	}
	if err := os.Chmod(path, cfg.SocketMode.Perm()); err != nil {
		return nil, errors.Join(fmt.Errorf("server: chmod socket: %w", err), l.Close())
	}
	return l, nil
}

// removeStaleSocket clears a socket left by a previous process. One that
// still answers belongs to a running server and is left alone.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("server: inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("server: %s exists and is not a socket", path)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		_ = conn.Close()
		return fmt.Errorf("server: %s is in use by another server", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("server: remove stale socket: %w", err)
	}
	return nil
}
