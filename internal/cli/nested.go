package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/driver"
)

// resolveSandboxRoot settles the sandbox root a command works under when
// neither its flag nor the configuration file names one. On a host it is
// the flag's default. Inside a machine — a worker that is itself a run's
// session, which `systemd-detect-virt --container` reports — it is
// [machine.NestedRoot], a subvolume under the machine's own ephemeral root,
// made here as the worker's own uid; a nested worker's lineages live there
// and nowhere the machine that booted it could promote from
// (docs/sandbox.md §Self-hosting).
func resolveSandboxRoot(cmd *cobra.Command, root *string, configured string) error {
	return resolveSandboxRootOn(cmd, cacheDriver(), machine.NestedRoot, root, configured)
}

// resolveSandboxRootOn is resolveSandboxRoot against a given host and a
// given nested root. The root is a parameter because
// [driver.EnsureNestedRoot] asks the filesystem whether it is already
// there, and inside a machine where a worker has run — where mill's own
// live tier runs — it is: the spec that proves the create is asked of the
// driver needs a root no one has made yet.
func resolveSandboxRootOn(cmd *cobra.Command, host driver.Driver, nested string, root *string, configured string) error {
	if !unset(cmd, "sandbox-root") {
		return nil
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if configured != "" {
		*root = configured
		return nil
	}
	if !machine.InsideMachine(ctx, host) {
		return nil
	}
	*root = nested
	if err := driver.EnsureNestedRoot(ctx, host, *root); err != nil {
		return fmt.Errorf("mill: nested sandbox root: %w", err)
	}
	return nil
}
