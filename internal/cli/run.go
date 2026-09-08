package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/claudecode"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
)

// ExitFenced is the status `mill run` exits with when another worker took
// the node's lease. It is distinct from an ordinary failure because nothing
// went wrong: the ledger is the truth, the new holder owns the run, and a
// supervisor restarting on this is restarting on the normal path.
const ExitFenced = 75

// daggerSocketEnv and daggerProjectEnv are dagger's own variables, so a
// shell already set up for `dg` needs nothing more for `mill run`.
const (
	daggerSocketEnv  = "DG_SOCKET"
	daggerProjectEnv = "DG_PROJECT"
)

// defaultDaggerSocket is where dagger listens when DG_SOCKET says nothing.
const defaultDaggerSocket = "/run/dagger/dg.sock"

// The forge credential the mill holds for the repositories it delivers to.
// It is read from the environment rather than from a flag so it never
// reaches a process listing, and it is the bottom rung of the credential
// ladder until a secrets service can mint one per step
// (docs/architecture.md §"Identity and credentials").
const (
	forgeTokenEnv = "MILL_FORGE_TOKEN" //nolint:gosec // the name of a variable, not a credential
	forgeUserEnv  = "MILL_FORGE_USER"
	vcsURLEnv     = "MILL_VCS_URL"
)

// sidecarEnv names the harness sidecar's entrypoint. The adapter's default
// is relative, and a session's working directory is the run's checkout —
// some other project's repository — so a mill installed anywhere but its own
// source tree has to be told where its sidecar is.
const sidecarEnv = "MILL_SIDECAR"

// defaultForgeUser is the basic-auth username GitLab wants beside a token.
const defaultForgeUser = "mill"

type runOptions struct {
	socket       string
	daggerSocket string
	project      string
	workflow     string
	tier         string
	workflowDir  string
	sidecar      string
	vcsURL       string
	origin       string
	ref          string
	identity     string
	name         string
	workspaceDir string
	ttl          time.Duration
	interval     time.Duration
	dryRun       bool
	noAsk        bool
}

// newRunCommand is "work" with the claim replaced by an argument: one node,
// driven in the calling process, on the code path the daemon uses (docs/prd.md
// R-45).
func newRunCommand() *cobra.Command {
	opts := runOptions{}
	cmd := &cobra.Command{
		Use:   "run <node>",
		Short: "Drive a single node's run in this process",
		Long: `Drive one dagger node end to end: claim it, hold its lease, walk the
selected workflow's steps, and complete it with a result.

This is the worker's own code path with the claim replaced by an argument.
The node must be ready; a node that is not is explained and the command
stops rather than waiting for it.

--dry-run claims nothing and executes nothing. It prints the workflow that
would be selected, its steps in order, and the evidence each one closes on.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.dryRun {
				return dryRun(cmd, opts, args[0])
			}
			return execute(cmd, opts, args[0])
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket")
	f.StringVar(&opts.daggerSocket, "dagger-socket", envOr(daggerSocketEnv, defaultDaggerSocket), "path of dagger's unix socket (default: $"+daggerSocketEnv+")")
	f.StringVar(&opts.project, "project", os.Getenv(daggerProjectEnv), "dagger project a bare #7 reference resolves in (default: $"+daggerProjectEnv+")")
	f.StringVar(&opts.workflow, "workflow", "quick-fix", "workflow to select when the run has none")
	f.StringVar(&opts.tier, "tier", "quick-fix", "tier that workflow stands for, which gate policies compare against")
	f.StringVar(&opts.workflowDir, "workflow-dir", "", "directory the workflow was loaded from, for a check step's scripts")
	f.StringVar(&opts.sidecar, "sidecar", envOr(sidecarEnv, claudecode.DefaultEntrypoint), "path of the harness sidecar's entrypoint (default: $"+sidecarEnv+")")
	f.StringVar(&opts.vcsURL, "vcs-url", os.Getenv(vcsURLEnv), "repository this project delivers to, which the forge steps push and merge against (default: $"+vcsURLEnv+")")
	f.StringVar(&opts.origin, "origin", "", "repository a new workspace clones (default: the --vcs-url repository)")
	f.StringVar(&opts.ref, "ref", "", "ref a new workspace checks out (default: the repository's own HEAD)")
	f.StringVar(&opts.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces are made under")
	f.DurationVar(&opts.ttl, "ttl", run.DefaultTTL, "lease TTL to ask dagger for")
	f.DurationVar(&opts.interval, "heartbeat", 0, "how often to renew the lease (default: a third of the TTL)")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the workflow and its steps; claim nothing and execute nothing")
	f.StringVar(&opts.identity, "identity", "", "address the mill commits under, and the principal of its allowed_signers line (default: mill@<host>)")
	f.StringVar(&opts.name, "name", defaultForgeUser, "name on the mill's commits")
	f.BoolVar(&opts.noAsk, "no-ask", false, "leave a filed gate for a human elsewhere instead of asking on this terminal")
	return cmd
}

// dryRun prints what a run would do. It reads the workflow and nothing else:
// no claim, no lease, no run, no workspace, no session.
func dryRun(cmd *cobra.Command, opts runOptions, nodeRef string) error {
	store := storeClient(opts.socket)
	workflow, err := selectedWorkflow(cmd.Context(), store, opts)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s would run workflow %s@%s\n\n", nodeRef, workflow.GetName(), workflow.GetVersion())
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STEP\tKIND\tEVIDENCE\tCAPABILITIES")
	for _, step := range workflow.GetSteps() {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			step.GetName(), stepKindName(step.GetKind()), evidenceKindName(step.GetEvidence()),
			strings.Join(capabilityNames(step.GetCapabilities()), ","))
	}
	return w.Flush()
}

// selectedWorkflow is the version a run on this project would follow.
func selectedWorkflow(ctx context.Context, store millv1connect.StoreServiceClient, opts runOptions) (*millv1.Workflow, error) {
	// The scope is left unspecified so the store resolves project over org,
	// which is the precedence a run itself follows.
	res, err := store.GetWorkflow(ctx, connect.NewRequest(&millv1.GetWorkflowRequest{
		Name:    opts.workflow,
		Project: opts.project,
	}))
	if err != nil {
		return nil, err
	}
	if res.Msg.GetWorkflow() == nil {
		return nil, fmt.Errorf("mill run: no workflow named %q is stored", opts.workflow)
	}
	return res.Msg.GetWorkflow(), nil
}

// execute drives the node.
func execute(cmd *cobra.Command, opts runOptions, nodeRef string) error {
	if opts.project == "" {
		return fmt.Errorf("mill run: a dagger project is required; pass --project or set %s", daggerProjectEnv)
	}
	worker, err := workerRef()
	if err != nil {
		return err
	}
	client := dagger.Dial(opts.daggerSocket, opts.project,
		dagger.WithAgentRef(worker),
		dagger.WithSessionRef(nodeRef),
		dagger.WithTTL(opts.ttl))

	harnesses := harness.NewRegistry()
	if err := harnesses.Register(run.DefaultHarness,
		claudecode.New(claudecode.WithEntrypoint(claudecode.DefaultCommand, opts.sidecar))); err != nil {
		return err
	}

	// A gate filed under `mill run` is put to whoever is watching. The run
	// stops either way — the gate is filed, the lease released — and the
	// answer is written to dagger as theirs; answering just saves them
	// finding it in another window (docs/domain-model.md §Gate).
	var answers run.Answerer
	if !opts.noAsk {
		answers = NewTerminalAnswerer(cmd.InOrStdin(), cmd.OutOrStdout())
	}

	tier, err := tierNamed(opts.tier)
	if err != nil {
		return err
	}

	// The forge is resolved once and shared: the loop reads the pull request
	// through it and the workspace pushes through it, and both must be
	// talking to the same repository. A project with no VCS URL yields a
	// *forge.NoVCSURLError here, before anything is claimed.
	f, err := projectForge(opts)
	if err != nil {
		return err
	}
	if opts.origin == "" {
		repo, err := forge.RepoFor(forge.Project{Slug: opts.project, VCSURL: opts.vcsURL})
		if err != nil {
			return err
		}
		opts.origin = repo.CloneURL
	}
	if opts.identity == "" {
		opts.identity = defaultIdentity()
	}

	runner := run.New(run.Options{
		Store:       storeClient(opts.socket),
		Dagger:      client,
		Workspaces:  localWorkspaces(opts, f),
		Forges:      func(context.Context) (forge.Forge, error) { return f, nil },
		Harnesses:   harnesses,
		WorkerRef:   worker,
		Workflow:    opts.workflow,
		Tier:        tier,
		WorkflowDir: opts.workflowDir,
		Origin:      opts.origin,
		Ref:         opts.ref,
		TTL:         opts.ttl,
		Interval:    opts.interval,
		Answers:     answers,
		Out:         cmd.OutOrStdout(),
		Log:         slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	return runner.Execute(cmd.Context(), nodeRef)
}

// tierNamed reads the project default's tier. It is a flag rather than a
// lookup because dagger's projects carry no tier and mill stores no project
// row yet; the assessment step that would name one is the router's work
// (docs/domain-model.md §"Workflow selection").
func tierNamed(name string) (millv1.Tier, error) {
	tier := millv1.Tier(millv1.Tier_value["TIER_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))])
	if tier == millv1.Tier_TIER_UNSPECIFIED {
		return tier, fmt.Errorf("mill run: %q is not a tier: quick-fix, small, feature or large", name)
	}
	return tier, nil
}

// localWorkspaces makes a workspace under the worker's own account,
// reattaching one this worker left behind before cloning a new one. The
// forge goes in with it, because the `push` and `pr` verbs a session reaches
// over the socket are the workspace's and not the loop's.
func localWorkspaces(opts runOptions, f forge.Forge) run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec) (run.Workspace, error) {
		ws := local.New(local.Options{
			Root:     opts.workspaceDir,
			Origin:   opts.origin,
			Forge:    f,
			Name:     opts.name,
			Identity: opts.identity,
		})
		if _, err := ws.Attach(ctx, spec); err == nil {
			return ws, nil
		}
		if _, err := ws.Create(ctx, spec); err != nil {
			return nil, err
		}
		return ws, nil
	}
}

// forges is the registry `mill run` resolves a project's forge through.
// Registering a forge is the only place an implementation is named
// (docs/roadmap.md §"Adding a forge is configuration").
func forges() *forge.Registry {
	registry := forge.NewRegistry()
	registry.Register(forge.KindGitLab, func(repo forge.Repo, minter forge.Minter) (forge.Forge, error) {
		return gitlab.New(repo, minter), nil
	})
	return registry
}

// projectForge is where the run delivers, resolved once at startup so a
// project with no repository fails before a workspace is created rather than
// four steps in.
//
// dagger's project model carries no VCS URL and no labels of its own
// (dagger's docs/domain-model.md §Project), so O0 takes it from --vcs-url or
// $MILL_VCS_URL and forge.Project carries it in the field that exists for
// the day dagger does. The credential is the one the mill account holds,
// handed out per request by [forge.Static].
func projectForge(opts runOptions) (forge.Forge, error) {
	return forges().For(
		forge.Project{Slug: opts.project, VCSURL: opts.vcsURL},
		forge.Static(envOr(forgeUserEnv, defaultForgeUser), os.Getenv(forgeTokenEnv)))
}

// workerRef names this worker on the run and on every lease: the account and
// the host it runs on.
func workerRef() (string, error) {
	who, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("mill run: identify the worker: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("mill run: identify the worker: %w", err)
	}
	return who.Username + "@" + host, nil
}

// defaultIdentity is the address the mill commits under when nothing names
// one: the account and host that own the signing key.
func defaultIdentity() string {
	ref, err := workerRef()
	if err != nil {
		return defaultForgeUser + "@localhost"
	}
	return ref
}

// defaultWorkspaceDir is where run workspaces live under the worker's
// account.
func defaultWorkspaceDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "mill", "workspaces")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "mill", "workspaces")
	}
	return filepath.Join(home, ".local", "state", "mill", "workspaces")
}

// envOr is a variable's value or a fallback.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// stepKindName, evidenceKindName and capabilityNames render enums for the
// dry run's table.
func stepKindName(kind millv1.StepKind) string {
	switch kind {
	case millv1.StepKind_STEP_KIND_SKILL:
		return "skill"
	case millv1.StepKind_STEP_KIND_CHECK:
		return "check"
	case millv1.StepKind_STEP_KIND_GATE:
		return "gate"
	default:
		return "unspecified"
	}
}

func evidenceKindName(kind millv1.EvidenceKind) string {
	return strings.ToLower(strings.TrimPrefix(kind.String(), "EVIDENCE_KIND_"))
}

func capabilityNames(capabilities []millv1.Capability) []string {
	names := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		names = append(names, strings.ToLower(strings.TrimPrefix(capability.String(), "CAPABILITY_")))
	}
	return names
}
