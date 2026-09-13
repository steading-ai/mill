package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
)

type restartOptions struct {
	socket       string
	daggerSocket string
	project      string
	workspaceDir string
	reason       string
}

// newRestartCommand abandons a node's run and starts a new generation. It is
// human-only: no worker reaches for it, because the circuit breaker parks and
// a human restarts (docs/domain-model.md §Restart).
func newRestartCommand() *cobra.Command {
	opts := restartOptions{}
	cmd := &cobra.Command{
		Use:   "restart <node>",
		Short: "Abandon a node's run and start a new generation",
		Long: `Abandon the live run on a node and open the next generation.

The lease is released as preempted with detail "restart" and any running step
is cancelled; the run is marked abandoned, with its step runs, evidence and
usage kept as the record of what went wrong; the workspace is destroyed, the
branch deleted, and the pull request closed with a comment naming the
restart. The new generation's session ids derive from the generation, so
every session in it is one the harness has never seen.

Never automatic. A step that fails repeatedly parks the run and costs a human
decision; this is that decision.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.project == "" {
				return fmt.Errorf("mill restart: a dagger project is required; pass --project or set %s", daggerProjectEnv)
			}
			worker, err := workerRef()
			if err != nil {
				return err
			}
			runner := run.New(run.Options{
				Store:  storeClient(opts.socket),
				Dagger: dagger.Dial(opts.daggerSocket, opts.project, dagger.WithAgentRef(worker)),
				// Attach-only: a restart destroys the workspace the run left
				// behind and must never clone one in order to delete it.
				Workspaces:   attachedWorkspaces(opts.workspaceDir),
				Repositories: repositories(runOptions{project: opts.project}),
				WorkerRef:    worker,
				Out:          cmd.OutOrStdout(),
			})
			_, err = runner.Restart(cmd.Context(), args[0], opts.reason)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	f.StringVar(&opts.daggerSocket, "dagger-socket", envOr(daggerSocketEnv, defaultDaggerSocket), "path of dagger's unix socket (default: $"+daggerSocketEnv+")")
	f.StringVar(&opts.project, "project", os.Getenv(daggerProjectEnv), "dagger project a bare #7 reference resolves in (default: $"+daggerProjectEnv+")")
	f.StringVar(&opts.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces live under")
	f.StringVar(&opts.reason, "reason", "restart", "why, for the lease's release detail and the ledger")
	return cmd
}

// attachedWorkspaces finds a workspace this host already has and never makes
// one. Restart is the caller: cloning a repository in order to delete it
// would be absurd, and a run worked on another host has nothing here to
// destroy.
func attachedWorkspaces(root string) run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec, _ run.Repository) (run.Workspace, error) {
		ws := local.New(local.Options{Root: root})
		if _, err := ws.Attach(ctx, spec); err != nil {
			return nil, err
		}
		return ws, nil
	}
}
