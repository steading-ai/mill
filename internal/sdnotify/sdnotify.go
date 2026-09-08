// Package sdnotify tells systemd how the service is doing.
//
// Under a Type=notify unit, systemd passes the path of a datagram socket in
// NOTIFY_SOCKET and treats the unit as started only when the process sends
// READY=1 on it. Outside systemd the variable is unset and every call here
// is a no-op that reports so, which keeps the service indifferent to how it
// was launched. The protocol is sd_notify(3).
package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Env is the variable systemd puts the notification socket's path in.
const Env = "NOTIFY_SOCKET"

// States the service sends.
const (
	// Ready tells systemd startup has finished and the service can take
	// work. For mill, that is once the database has answered.
	Ready = "READY=1"
	// Stopping tells systemd the service has begun shutting down.
	Stopping = "STOPPING=1"
)

// Status is a free-text state line systemd shows in `systemctl status`.
func Status(text string) string {
	return "STATUS=" + strings.ReplaceAll(text, "\n", " ")
}

// Send writes one or more state lines to the notification socket. It returns
// false and no error when the process is not running under a notify unit.
// The socket path comes from the environment on every call so that a spec
// can point it somewhere of its own.
func Send(states ...string) (bool, error) {
	return send(os.Getenv(Env), states...)
}

func send(path string, states ...string) (bool, error) {
	if path == "" || len(states) == 0 {
		return false, nil
	}
	// A leading @ names a socket in the abstract namespace, which the
	// kernel spells with a leading NUL.
	if path[0] == '@' {
		path = "\x00" + path[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return false, fmt.Errorf("sdnotify: %w", err)
	}
	defer conn.Close() //nolint:errcheck // nothing to do about a failed close of a datagram socket
	if _, err := conn.Write([]byte(strings.Join(states, "\n") + "\n")); err != nil {
		return false, fmt.Errorf("sdnotify: %w", err)
	}
	return true, nil
}
