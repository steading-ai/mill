// Package cli assembles the cobra command tree for the mill binary.
//
// One binary has two faces: the service that owns the store and its API, and
// the worker that claims a dagger node and drives its run. See the "One
// binary, two faces" section of docs/architecture.md. The subcommands here
// are the faces themselves plus "version"; each carries no behaviour yet and
// the nodes that follow the scaffold fill them in.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/version"
)

// Run executes the binary with args and returns its exit status. main is a
// call to this.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := NewRootCommand(stdout, stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "mill:", err)
		return ExitCode(err)
	}
	return 0
}

// ExitCode is the status one failure exits with. Being fenced out has a
// status of its own because nothing went wrong: another worker holds the
// node, the ledger is the truth, and a supervisor that restarts on this is
// restarting on the normal path rather than on an error.
func ExitCode(err error) int {
	if errors.Is(err, run.ErrFenced) {
		return ExitFenced
	}
	return 1
}

// NewRootCommand builds the mill command tree. Output is written to stdout
// and diagnostics to stderr; callers, including specs, may redirect either.
func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:     "mill",
		Short:   "A workflow runner over dagger's work graph",
		Version: version.String(),
		Long: `mill is the single entry point to the workflow runner.

"mill serve" runs the service: the store, the workflows, and the ledger.
"mill work" runs a worker: claim a ready dagger node, then drive its run.
"mill run" drives a single named node in the calling process.
"mill restart" abandons a node's run and starts a new generation.
"mill rerun" redoes a run from a step, keeping the generation and sessions.
"mill workspace" shows and cleans up the checkouts runs leave on this host.
"mill image" builds the base image and lists and prunes image subvolumes.
"mill workflow" pushes, pulls, shows, and lists workflow definitions.
"mill status", "inspect", "explain", "logs", "history" and "cost" read the
ledger; "mill doctor" probes what a run depends on; "mill cancel" stops one.
"mill mcp" serves those same tools to a session over stdio.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Every face that dials resolves its sockets here, before it
		// runs, so the configuration file reaches all of them through
		// one resolver (sockets.go).
		PersistentPreRunE: applySocketConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(
		newVersionCommand(),
		newServeCommand(),
		newWorkCommand(),
		newRunCommand(),
		newRestartCommand(),
		newRerunCommand(),
		newWorkflowCommand(),
		newWorkspaceCommand(),
		newImageCommand(),
		newCacheCommand(),
		newStatusCommand(),
		newInspectCommand(),
		newExplainCommand(),
		newLogsCommand(),
		newHistoryCommand(),
		newCostCommand(),
		newCancelCommand(),
		newDoctorCommand(),
		newMCPCommand(),
	)
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this build",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "mill version %s\n", version.String())
			return err
		},
	}
}
