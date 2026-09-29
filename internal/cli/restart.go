package cli

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/steading-ai/mill/gen/mill/v1"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/skift"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
	"github.com/steading-ai/mill/internal/workspace/redact"
)

type restartOptions struct {
	config      string
	socket      string
	skiftSocket string
	project     string
	reason      string
	// run carries what the workspace the restart destroys is found by: the
	// backend, its roots, and the identity the checkout was made under.
	run runOptions
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

The workspace is found on the backend the configuration names — the machine
a run booted, or the checkout under the workspace directory — and never made
in order to be destroyed. A node that is not open has nothing to restart.

Never automatic. A step that fails repeatedly parks the run and costs a human
decision; this is that decision.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.project == "" {
				return fmt.Errorf("mill restart: a skift project is required; pass --project or set %s", skiftProjectEnv)
			}
			if err := applyRestartConfig(cmd, &opts); err != nil {
				return err
			}
			kind, err := workspace.ParseKind(opts.run.workspace)
			if err != nil {
				return fmt.Errorf("mill restart: --workspace: %w", err)
			}
			worker, err := workerRef()
			if err != nil {
				return err
			}
			opts.run.project = opts.project
			if opts.run.identity == "" {
				opts.run.identity = defaultIdentity()
			}
			runner := run.New(run.Options{
				Store: storeClient(opts.socket),
				Skift: skift.Dial(opts.skiftSocket, opts.project, skift.WithAgentRef(worker)),
				// Attach-only: a restart destroys the workspace the run left
				// behind and must never clone or boot one in order to
				// delete it.
				Attach:       attachOnlyWorkspaces(kind, opts.run, redact.New()),
				Repositories: repositories(opts.run),
				WorkerRef:    worker,
				Out:          cmd.OutOrStdout(),
				RetireBranch: retireBranchOverSocket(opts.socket, opts.project),
			})
			_, err = runner.Restart(cmd.Context(), args[0], opts.reason)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file, for the workspace backend and its roots (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml, else /etc/mill/config.yaml)")
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	f.StringVar(&opts.skiftSocket, "skift-socket", envOr(skiftSocketEnv, defaultSkiftSocket), "path of skift's unix socket (default: $"+skiftSocketEnv+")")
	f.StringVar(&opts.project, "project", os.Getenv(skiftProjectEnv), "skift project a bare #7 reference resolves in (default: $"+skiftProjectEnv+")")
	f.StringVar(&opts.run.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces live under")
	f.StringVar(&opts.run.workspace, "workspace", string(workspace.KindLocal), workspaceUsage)
	f.StringVar(&opts.run.sandboxRoot, "sandbox-root", machine.DefaultRoot, sandboxRootUsage)
	f.StringVar(&opts.reason, "reason", "restart", "why, for the lease's release detail and the ledger")
	return cmd
}

// retireBranchOverSocket is run.Options.RetireBranch for `mill restart`: the
// close-then-delete sequence runs inside the worker at socket, over
// WorkerService.RetireBranch, rather than resolving a forge credential in
// this process — an operator's shell restarting a run usually holds none of
// its own (docs/architecture.md §"Identity and credentials"). A socket with
// no worker mounted, or one that refuses, fails the restart naming the
// cause rather than falling back to a local credential: the incident this
// guards against was a silent partial success, and a fallback that
// sometimes resolves locally reintroduces the same ambiguity.
func retireBranchOverSocket(socket, project string) func(ctx context.Context, branch, comment string) (string, error) {
	return func(ctx context.Context, branch, comment string) (string, error) {
		res, err := workerClient(socket).RetireBranch(ctx, connect.NewRequest(&millv1.RetireBranchRequest{
			Project: project,
			Branch:  branch,
			Comment: comment,
		}))
		if err != nil {
			return "", fmt.Errorf("closing %s's forge state through the worker at %s: %w", branch, socket, err)
		}
		return res.Msg.GetPullRequestUrl(), nil
	}
}

// applyRestartConfig fills in, from the configuration file, what finding
// the run's workspace depends on and the command line left alone: the
// backend, the workspace directory, the sandbox root and the machine
// backend's settings, and the identity the checkout was made under. It is
// the worker's own file, read the way the worker reads it, so a restart
// looks where the worker put things.
func applyRestartConfig(cmd *cobra.Command, opts *restartOptions) error {
	if err := checkNamedConfig(cmd, opts.config); err != nil {
		return err
	}
	config, err := LoadConfig(opts.config)
	if err != nil {
		return err
	}
	for name, setting := range map[string]struct {
		target *string
		value  string
	}{
		"workspace":     {&opts.run.workspace, config.Workspace},
		"workspace-dir": {&opts.run.workspaceDir, config.WorkspaceDir},
	} {
		if setting.value != "" && unset(cmd, name) {
			*setting.target = setting.value
		}
	}
	opts.run.identity = config.Identity
	opts.run.name = config.Name
	if opts.run.name == "" {
		opts.run.name = defaultForgeUser
	}
	opts.run.workflowDir = config.WorkflowDir
	if err := resolveSandboxRoot(cmd, &opts.run.sandboxRoot, config.SandboxRoot); err != nil {
		return err
	}
	return applySandboxConfig(config, &opts.run.sandbox)
}
