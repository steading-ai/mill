package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// Where a face finds the service and dagger.
//
// Every command that dials the service's socket or dagger's resolves each
// path the same way, in one order: the flag, then the variable ($MILL_SOCKET,
// $DG_SOCKET), then the configuration file's `socket` and `dagger-socket`,
// then the default. The file sits below the environment on purpose: a
// deployment that sets $MILL_SOCKET on a unit or a plugin registration keeps
// reaching the service it named, whatever a file on the host says, and a
// person on a host with no environment at all — the Mac-side CLI reaching a
// daemon through forwarded sockets — types nothing.
//
// The flags' declared defaults already carry the first two rungs, so the
// resolver has one job: fill the file's value in below them.

// socketKeys are the configuration keys the resolver reads, each with the
// flag it fills and the variable that outranks it.
var socketKeys = []struct{ flag, env string }{
	{"socket", socketEnv},
	{"dagger-socket", daggerSocketEnv},
}

// dialsSockets reports whether a command declares either socket flag, which
// is what makes it one the resolver applies to.
func dialsSockets(cmd *cobra.Command) bool {
	for _, key := range socketKeys {
		if cmd.Flags().Lookup(key.flag) != nil {
			return true
		}
	}
	return false
}

// resolveSockets fills in, from the configuration file, each socket flag the
// command declares, the command line left alone, and the environment says
// nothing about. It writes through the flag, so whatever variable the flag
// is bound to sees the value, and marks it set, so a second pass — `work`
// applies its file after the root has — changes nothing.
func resolveSockets(cmd *cobra.Command, config Config) error {
	values := map[string]string{"socket": config.Socket, "dagger-socket": config.DaggerSocket}
	for _, key := range socketKeys {
		value := values[key.flag]
		if value == "" || !unset(cmd, key.flag) || os.Getenv(key.env) != "" {
			continue
		}
		if err := cmd.Flags().Set(key.flag, value); err != nil {
			return err
		}
	}
	return nil
}

// applySocketConfig is the root's pre-run: for a command that dials, read
// the configuration file — the one --config names when the command declares
// it, else the default location — and resolve its sockets from it. A file
// that is not there says nothing, here as everywhere the file is read;
// whether a named file that is missing is refused stays each command's own
// decision (checkNamedConfig), because `doctor` is run against a file that
// is not there on purpose and a worker is not.
func applySocketConfig(cmd *cobra.Command, _ []string) error {
	if !dialsSockets(cmd) {
		return nil
	}
	path := defaultConfigPath()
	if flag := cmd.Flags().Lookup("config"); flag != nil {
		path = flag.Value.String()
	}
	config, err := LoadConfig(path)
	if err != nil {
		return err
	}
	return resolveSockets(cmd, config)
}
