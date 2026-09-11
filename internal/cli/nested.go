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
	return resolveSandboxRootOn(cmd, cacheDriver(), root, configured)
}

// resolveSandboxRootOn is resolveSandboxRoot against a given host.
func resolveSandboxRootOn(cmd *cobra.Command, host driver.Driver, root *string, configured string) error {
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
	*root = machine.NestedRoot
	if err := driver.EnsureNestedRoot(ctx, host, *root); err != nil {
		return fmt.Errorf("mill: nested sandbox root: %w", err)
	}
	return nil
}
