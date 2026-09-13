package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	daggerv1 "github.com/ivy/mill/gen/dagger/v1"
	millv1 "github.com/ivy/mill/gen/mill/v1"
	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/dagger"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/forge/gitlab"
	"github.com/ivy/mill/internal/gateway"
	"github.com/ivy/mill/internal/harness"
	"github.com/ivy/mill/internal/harness/claude"
	"github.com/ivy/mill/internal/notify"
	"github.com/ivy/mill/internal/run"
	"github.com/ivy/mill/internal/skill"
	"github.com/ivy/mill/internal/version"
	"github.com/ivy/mill/internal/workflow"
	"github.com/ivy/mill/internal/workspace"
	"github.com/ivy/mill/internal/workspace/local"
	"github.com/ivy/mill/internal/workspace/machine"
	"github.com/ivy/mill/internal/workspace/machine/cache"
	"github.com/ivy/mill/internal/workspace/machine/driver"
	"github.com/ivy/mill/internal/workspace/machine/image"
	"github.com/ivy/mill/internal/workspace/redact"
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
	// The forge names this one, because the forge is what refuses a project
	// with no repository and its refusal has to say where a URL comes from.
)

// sidecarEnv names the harness sidecar's entrypoint. The adapter's default
// is relative, and a session's working directory is the run's checkout —
// some other project's repository — so a mill installed anywhere but its own
// source tree has to be told where its sidecar is.
const sidecarEnv = "MILL_SIDECAR"

// envModeUsage is what --env-mode says for itself on every command that has
// it. The modes themselves are internal/harness/claude/environment.go.
const envModeUsage = "environment a session's sidecar is spawned with: " +
	"inherit (the mill's own, minus the deny list) or strict (a floor plus what a step names)"

// workspaceUsage is what --workspace says for itself on every command that
// has it. The kinds are internal/workspace's, and the table is
// docs/architecture.md §"The workspace".
const workspaceUsage = "workspace a run works in: " +
	"local (a clone under the mill's account) or machine (a systemd-nspawn machine per run; docs/sandbox.md)"

// sandboxRootUsage is what --sandbox-root says for itself.
const sandboxRootUsage = "the machine workspace's sandbox root, a btrfs volume holding runs, caches and images; " +
	"read only under --workspace machine"

// The scripted-implementer seam. `--skill-override <step>=<dir>` runs a step
// on a skill read from a directory instead of the one the workflow pinned in
// the store, which is how the acceptance suite seeds an implementation that
// compiles, lints and violates a stated criterion (docs/prd.md R-25,
// docs/runbook.md §"The scripted implementer").
//
// It exists for that test and for nothing else, so it is refused unless the
// acceptance suite's own variable is set. A run driven with an unpinned
// prompt would produce evidence nothing can be traced back to a pushed
// workflow, and the refusal happens in the command rather than in the loop
// so that nothing is claimed and no lease is taken first.
const (
	acceptanceEnv = "MILL_ACCEPTANCE"

	skillOverrideUsage = "run a step on a skill read from a directory instead of the workflow's: " +
		"<step>=<dir>. A test seam for the acceptance suite; refused unless $" + acceptanceEnv + " is set"
)

// checkSkillOverrides refuses the seam outside the acceptance suite.
func checkSkillOverrides(overrides map[string]string) error {
	if len(overrides) == 0 || os.Getenv(acceptanceEnv) != "" {
		return nil
	}
	return fmt.Errorf("mill run: --skill-override is a test seam for the acceptance suite, "+
		"not a way to run a prompt the store does not hold; it is refused unless %s is set. "+
		"Nothing was claimed", acceptanceEnv)
}

// skillOverrides reads each override's directory into the skill the step
// will run. The directory is a skill directory as `mill workflow push`
// accepts one, so a seam and a real skill are the same kind of thing and the
// seam cannot run something the loader would reject.
func skillOverrides(overrides map[string]string, out io.Writer) (map[string]*millv1.Skill, error) {
	if len(overrides) == 0 {
		return nil, nil
	}
	loaded := make(map[string]*millv1.Skill, len(overrides))
	for _, step := range slices.Sorted(maps.Keys(overrides)) {
		dir := overrides[step]
		skill, err := workflow.LoadSkill(dir)
		if err != nil {
			return nil, fmt.Errorf("mill run: --skill-override %s=%s: %w", step, dir, err)
		}
		loaded[step] = skill
		_, _ = fmt.Fprintf(out, "warning: step %s runs the skill in %s, not the one its workflow pins: "+
			"--skill-override is a test seam\n", step, dir)
	}
	return loaded, nil
}

// defaultForgeUser is the basic-auth username GitLab wants beside a token.
const defaultForgeUser = "mill"

type runOptions struct {
	socket        string
	daggerSocket  string
	project       string
	workflow      string
	tier          string
	workflowDir   string
	sidecar       string
	origin        string
	ref           string
	identity      string
	name          string
	workspaceDir  string
	workspace     string
	sandboxRoot   string
	sandbox       sandboxOptions
	logDir        string
	envMode       string
	notifyCommand string
	skillOverride map[string]string
	gateway       gatewayOptions
	ttl           time.Duration
	interval      time.Duration
	dryRun        bool
	noAsk         bool
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

--workflow pins this project to one workflow, which is the project default
of docs/domain-model.md §Selection. Left out, the run assesses the node: the
build-workflow skill runs as its first step and names the workflow and the
tier, and that assessment is the selection's evidence.

--dry-run claims nothing and executes nothing. It prints the workflow that
would be selected, its steps in order, and the evidence each one closes on —
or, when the run would assess, the workflows the assessment could choose
between.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSkillOverrides(opts.skillOverride); err != nil {
				return err
			}
			if opts.dryRun {
				return dryRun(cmd, opts, args[0])
			}
			return execute(cmd, opts, args[0])
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the service's unix socket (default: $"+socketEnv+")")
	f.StringVar(&opts.daggerSocket, "dagger-socket", envOr(daggerSocketEnv, defaultDaggerSocket), "path of dagger's unix socket (default: $"+daggerSocketEnv+")")
	f.StringVar(&opts.project, "project", os.Getenv(daggerProjectEnv), "dagger project a bare #7 reference resolves in (default: $"+daggerProjectEnv+")")
	f.StringVar(&opts.workflow, "workflow", "", "workflow to select when the run has none (default: assess the node and let the assessment name one)")
	f.StringVar(&opts.tier, "tier", "", "tier that workflow stands for, which gate policies compare against (default: quick-fix, or the assessed tier)")
	f.StringVar(&opts.workflowDir, "workflow-dir", "", "directory the workflow was loaded from, for a check step's scripts")
	f.StringVar(&opts.sidecar, "sidecar", envOr(sidecarEnv, claude.DefaultEntrypoint), "path of the harness sidecar's entrypoint (default: $"+sidecarEnv+")")
	f.StringVar(&opts.origin, "origin", "", "repository a new workspace clones (default: the project's repository)")
	f.StringVar(&opts.ref, "ref", "", "ref a new workspace checks out (default: the repository's own HEAD)")
	f.StringVar(&opts.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces are made under")
	f.StringVar(&opts.workspace, "workspace", string(workspace.KindLocal), workspaceUsage)
	f.StringVar(&opts.sandboxRoot, "sandbox-root", machine.DefaultRoot, sandboxRootUsage)
	f.StringVar(&opts.logDir, "log-dir", defaultLogDir(), "directory step logs are written under, one file per attempt")
	f.StringVar(&opts.envMode, "env-mode", string(claude.DefaultEnvMode), envModeUsage)
	f.DurationVar(&opts.ttl, "ttl", run.DefaultTTL, "lease TTL to ask dagger for")
	f.DurationVar(&opts.interval, "heartbeat", 0, "how often to renew the lease (default: a third of the TTL)")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the workflow and its steps; claim nothing and execute nothing")
	f.StringVar(&opts.identity, "identity", "", "address the mill commits under, and the principal of its allowed_signers line (default: mill@<host>)")
	f.StringVar(&opts.name, "name", defaultForgeUser, "name on the mill's commits")
	f.StringToStringVar(&opts.skillOverride, "skill-override", nil, skillOverrideUsage)
	f.StringVar(&opts.notifyCommand, "notify-command", "", notifyCommandUsage)
	f.BoolVar(&opts.noAsk, "no-ask", false, "leave a filed gate for a human elsewhere instead of asking on this terminal")
	bindGatewayFlags(cmd, &opts.gateway)
	return cmd
}

// notifyCommandUsage is what --notify-command says for itself on every
// command that has it. The seam is internal/notify: a command that receives
// the gate as JSON on its standard input and carries it wherever the
// deployment wants it carried.
const notifyCommandUsage = "command run with a filed gate as JSON on its standard input, " +
	"so a person hears about it; empty files the gate and tells nobody " +
	"(example: contrib/notify/thicket-send.sh)"

// dryRun prints what a run would do. It reads the workflow and nothing else:
// no claim, no lease, no run, no workspace, no session.
func dryRun(cmd *cobra.Command, opts runOptions, nodeRef string) error {
	store := storeClient(opts.socket)
	if strings.TrimSpace(opts.workflow) == "" {
		return dryRunAssessment(cmd, store, opts, nodeRef)
	}
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

// dryRunAssessment prints what selection would do for a project with no
// default: run the assessment, and choose between the workflows in scope.
// The list is the one the assessing session is given, so what an operator is
// shown here is what the model will be asked to pick from
// (internal/run/select.go).
func dryRunAssessment(cmd *cobra.Command, store millv1connect.StoreServiceClient, opts runOptions, nodeRef string) error {
	ctx := cmd.Context()
	assessor, err := store.GetWorkflow(ctx, connect.NewRequest(&millv1.GetWorkflowRequest{
		Name: run.AssessmentWorkflow, Project: opts.project,
	}))
	if err != nil {
		return err
	}
	if assessor.Msg.GetWorkflow() == nil {
		return fmt.Errorf("mill run: %s has no project default and no workflow named %q to assess with; "+
			"push it, or pin the project with --workflow", nodeRef, run.AssessmentWorkflow)
	}
	listed, err := store.ListWorkflows(ctx, connect.NewRequest(&millv1.ListWorkflowsRequest{}))
	if err != nil {
		return err
	}
	candidates := run.InScope(listed.Msg.GetWorkflows(), opts.project)

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s has no project default, so the run would assess it: %s@%s runs its %s step first "+
		"and names the workflow and the tier.\n\n",
		nodeRef, assessor.Msg.GetWorkflow().GetName(), assessor.Msg.GetWorkflow().GetVersion(), run.AssessmentStep)
	if len(candidates) == 0 {
		_, _ = fmt.Fprintln(out, "There is no workflow in scope for it to choose between.")
		return nil
	}
	_, _ = fmt.Fprintln(out, "It would choose between:")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "WORKFLOW\tVERSION")
	for _, workflow := range candidates {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", workflow.GetName(), workflow.GetVersion())
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
	if err := resolveSandboxRoot(cmd, &opts.sandboxRoot, ""); err != nil {
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

	gw, err := startGateway(cmd, opts.gateway)
	if err != nil {
		return err
	}
	if gw != nil {
		defer func() { _ = gw.Close() }()
	}
	runner, err := newRunner(cmd, opts, opts.project, nodeRef, answers, gw)
	if err != nil {
		return err
	}
	return runner.Execute(cmd.Context(), nodeRef)
}

// newRunner assembles the run loop for one project. It is shared by `run`
// and `work` on purpose: the daemon and single-run mode differ in where the
// node comes from and in whether a person can answer a gate, and in nothing
// else. If they diverged here, single-run mode would stop being a test of
// the daemon (docs/architecture.md §"One binary, two faces").
//
// sessionRef is what dagger records on the lease as the harness session.
// `mill run` names the node it was given; a worker has none to name, because
// the node is not known until the claim it is an argument to has returned.
//
// gw is the process's credential gateway, started once by the caller and
// shared by every runner, or nil when none is configured.
func newRunner(cmd *cobra.Command, opts runOptions, project, sessionRef string, answers run.Answerer, gw *gateway.Server) (*run.Runner, error) {
	// The first thing, because it is the cheapest thing that can be wrong
	// and the most expensive to find out late: a backend that cannot serve
	// a run on this host fails after the node has been claimed and cloned
	// unless it is asked first.
	kind, err := workspace.ParseKind(opts.workspace)
	if err != nil {
		return nil, fmt.Errorf("mill run: --workspace: %w", err)
	}
	if err := checkWorkspace(kind, opts); err != nil {
		return nil, err
	}
	worker, err := workerRef()
	if err != nil {
		return nil, err
	}
	client := dagger.Dial(opts.daggerSocket, project,
		dagger.WithAgentRef(worker),
		dagger.WithSessionRef(sessionRef),
		dagger.WithTTL(opts.ttl))

	envMode, err := claude.ParseEnvMode(opts.envMode)
	if err != nil {
		return nil, err
	}

	tier, err := tierNamed(opts.tier)
	if err != nil {
		return nil, err
	}

	overrides, err := skillOverrides(opts.skillOverride, cmd.ErrOrStderr())
	if err != nil {
		return nil, err
	}

	opts.project = project
	if opts.identity == "" {
		opts.identity = defaultIdentity()
	}

	// One redactor per run loop, shared by the workspace and the run's step
	// logs: a value a session registers with the `redact` verb reaches the
	// workspace, and the log an operator tails is written through the same
	// object (docs/architecture.md §Observability).
	redactor := redact.New()

	options := run.Options{
		Store:          storeClient(opts.socket),
		Notifier:       notifier(opts.notifyCommand),
		Dagger:         client,
		Workspaces:     workspacesFor(kind, opts, redactor),
		Repositories:   repositories(opts),
		Harnesses:      harnessesFor(kind, opts, envMode),
		WorkerRef:      worker,
		Workflow:       opts.workflow,
		Tier:           tier,
		WorkflowDir:    opts.workflowDir,
		Ref:            opts.ref,
		TTL:            opts.ttl,
		Interval:       opts.interval,
		Answers:        answers,
		Out:            cmd.OutOrStdout(),
		LogDir:         opts.logDir,
		Redactor:       redactor,
		SkillOverrides: overrides,
		Log:            slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	gatewayRunOptions(gw, &options)
	return run.New(options), nil
}

// notifier is the deployment's notify command, or nothing at all. It is a
// typed nil that must not reach the interface: a run whose Notifier is
// non-nil and empty would try to spawn nothing at every gate.
func notifier(command string) notify.Notifier {
	if parsed := notify.Parse(command); parsed != nil {
		return parsed
	}
	return nil
}

// tierNamed reads the project default's tier. It is a flag rather than a
// lookup because dagger's projects carry no tier and mill stores no project
// row yet (docs/domain-model.md §Selection). Empty leaves it unset: a run
// with a project default takes the documented default of quick-fix, and a
// run that assesses takes the tier the assessment named.
func tierNamed(name string) (millv1.Tier, error) {
	if name == "" {
		return millv1.Tier_TIER_UNSPECIFIED, nil
	}
	tier := millv1.Tier(millv1.Tier_value["TIER_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))])
	if tier == millv1.Tier_TIER_UNSPECIFIED {
		return tier, fmt.Errorf("mill run: %q is not a tier: quick-fix, small, feature or large", name)
	}
	return tier, nil
}

// sandboxOptions is what the machine backend reads from the configuration
// file beyond --sandbox-root: nothing here has a flag, because none of it
// changes between two runs of one worker. Empty means the backend's own
// default, which is what homestead fixes on `core`.
type sandboxOptions struct {
	machinesDir     string
	runStateDir     string
	skillDir        string
	sidecarDir      string
	workflowCopyDir string
	imageRegistry   string
	imageRepository string
	baseImage       string
	buildBudget     time.Duration
	globalCap       uint64
	globalCaps      map[string]uint64
	repoCap         uint64
	cacheExpiry     time.Duration
	highWatermark   cache.Watermark
	lowWatermark    cache.Watermark
	proxy           string
	noProxy         string
	nestedRepos     []string
	daggerCheckout  string
	// What the worker's start does to the host (docs/sandbox.md
	// §Reconciliation, §Eviction): the forge namespace and age the scratch
	// sweep works by, and the expiry image eviction works by.
	scratchNamespace string
	scratchBudget    time.Duration
	imageExpiry      time.Duration
}

// applySandboxConfig reads the machine backend's keys from the file.
func applySandboxConfig(config Config, opts *sandboxOptions) error {
	opts.machinesDir = config.MachinesDir
	opts.runStateDir = config.RunStateDir
	opts.skillDir = config.SkillDir
	opts.sidecarDir = config.SidecarDir
	opts.workflowCopyDir = config.WorkflowCopyDir
	opts.imageRegistry = config.ImageRegistry
	opts.imageRepository = config.ImageRepository
	opts.baseImage = config.BaseImage
	opts.proxy = config.Proxy
	opts.noProxy = config.NoProxy
	opts.nestedRepos = config.NestedRepos
	opts.daggerCheckout = config.DaggerCheckout
	opts.scratchNamespace = config.ScratchNamespace
	if config.ScratchBudget != "" {
		budget, err := time.ParseDuration(config.ScratchBudget)
		if err != nil {
			return fmt.Errorf("scratch-budget: %w", err)
		}
		opts.scratchBudget = budget
	}
	if config.ImageExpiry != "" {
		expiry, err := parseWindow(config.ImageExpiry)
		if err != nil {
			return fmt.Errorf("image-expiry: %q is not a window such as 720h or 30d", config.ImageExpiry)
		}
		opts.imageExpiry = expiry
	}
	if config.ImageBuildBudget != "" {
		budget, err := time.ParseDuration(config.ImageBuildBudget)
		if err != nil {
			return fmt.Errorf("image-build-budget: %w", err)
		}
		opts.buildBudget = budget
	}
	if config.CacheGlobalCap != "" {
		size, err := cache.ParseSize(config.CacheGlobalCap)
		if err != nil {
			return fmt.Errorf("cache-global-cap: %w", err)
		}
		opts.globalCap = size
	}
	if config.CacheRepoCap != "" {
		size, err := cache.ParseSize(config.CacheRepoCap)
		if err != nil {
			return fmt.Errorf("cache-repo-cap: %w", err)
		}
		opts.repoCap = size
	}
	caps, err := parseGlobalCaps(config.CacheGlobalCaps)
	if err != nil {
		return err
	}
	opts.globalCaps = caps
	return applyEvictionConfig(config, &opts.cacheExpiry, &opts.highWatermark, &opts.lowWatermark)
}

// parseGlobalCaps reads the per-lineage global caps.
func parseGlobalCaps(raw map[string]string) (map[string]uint64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	caps := make(map[string]uint64, len(raw))
	for name, text := range raw {
		size, err := cache.ParseSize(text)
		if err != nil {
			return nil, fmt.Errorf("cache-global-caps: %s: %w", name, err)
		}
		caps[name] = size
	}
	return caps, nil
}

// applyEvictionConfig reads the eviction policy's keys from the file into
// what the caller left zero (docs/sandbox.md §Eviction).
func applyEvictionConfig(config Config, expiry *time.Duration, high, low *cache.Watermark) error {
	if config.CacheExpiry != "" && *expiry == 0 {
		window, err := parseWindow(config.CacheExpiry)
		if err != nil {
			return fmt.Errorf("cache-expiry: %q is not a window such as 720h or 30d", config.CacheExpiry)
		}
		*expiry = window
	}
	if config.CacheHighWatermark != "" && high.IsZero() {
		mark, err := cache.ParseWatermark(config.CacheHighWatermark)
		if err != nil {
			return fmt.Errorf("cache-high-watermark: %w", err)
		}
		*high = mark
	}
	if config.CacheLowWatermark != "" && low.IsZero() {
		mark, err := cache.ParseWatermark(config.CacheLowWatermark)
		if err != nil {
			return fmt.Errorf("cache-low-watermark: %w", err)
		}
		*low = mark
	}
	return nil
}

// machineDriver is the host as a worker reaches it: every command through
// the chokepoint, the image directory through sudo because it is root's
// (docs/sandbox.md §Storage), and everything that enters a machine through
// the root-owned helper (§Privilege).
func machineDriver() driver.Driver {
	return driver.NewSystemd(workspace.Spawner{}, driver.Commands{Sudo: "sudo"})
}

// cacheDriver is the same host as the worker alone: lineages are the
// worker's subvolumes and never go through sudo (docs/sandbox.md §Caching).
func cacheDriver() driver.Driver {
	return driver.NewSystemd(workspace.Spawner{}, driver.Commands{})
}

// checkWorkspace is the backend's own preflight, asked before a node is
// claimed. For the local backend it is the socket path: a workspace
// directory whose run socket the kernel cannot bind fails on the bind, after
// the node has been claimed and cloned (internal/workspace/socket/path.go).
// For the machine backend it is whether this host can run one at all.
func checkWorkspace(kind workspace.Kind, opts runOptions) error {
	if kind == workspace.KindMachine {
		config := machineConfig(opts, run.Repository{}, nil)
		// The skill every machine mounts is this binary's, written where
		// the helper's policy admits a read-only bind from, with the
		// version beside it (docs/sandbox.md §"Skill mount").
		if err := skill.Materialise(config.SkillDir(), version.String()); err != nil {
			return fmt.Errorf("mill run: --workspace machine: %w", err)
		}
		// The sidecar rides in beside the skill: this binary's, materialised
		// where the helper's policy admits a read-only bind from, refreshed
		// only when the version differs (docs/sandbox.md §"Skill mount").
		if err := skill.MaterialiseSidecar(sidecarSourceDir(opts.sidecar), config.SidecarDir(), version.String()); err != nil {
			return fmt.Errorf("mill run: --workspace machine: %w", err)
		}
		// The workflow directory too, when one was named: a check step's
		// script is resolved under it on the host and runs inside the
		// machine, so the copy is what the machine has (docs/sandbox.md
		// §"Skill mount").
		if opts.workflowDir != "" {
			if err := skill.MaterialiseWorkflow(opts.workflowDir, config.WorkflowDir()); err != nil {
				return fmt.Errorf("mill run: --workspace machine: %w", err)
			}
		}
		if err := machine.Check(machineDriver(), config); err != nil {
			return fmt.Errorf("mill run: --workspace machine: %w", err)
		}
		return nil
	}
	if err := local.CheckRoot(opts.workspaceDir); err != nil {
		return fmt.Errorf("mill run: --workspace-dir: %w", err)
	}
	return nil
}

// harnessesFor builds the adapter table for one run, given the workspace it
// works in (docs/sandbox.md §Exec, §"Skill mount"). The choice is the
// backend's: the local backend spawns the sidecar on the host, exactly as it
// did before this node; the machine backend spawns it inside the run's
// machine through that workspace's Exec, so the session lives under the step
// slice, sees `/work` as `agent`, reaches the socket at its in-machine path,
// and dies with the step.
func harnessesFor(kind workspace.Kind, opts runOptions, envMode claude.EnvMode) run.HarnessesFor {
	return func(ws run.Workspace) (*harness.Registry, error) {
		registry := harness.NewRegistry()
		if err := registry.Register(run.DefaultHarness, claudeAdapter(kind, opts, envMode, ws)); err != nil {
			return nil, err
		}
		return registry, nil
	}
}

// claudeAdapter is the claude adapter as each backend needs it. The machine
// backend hands it the run's workspace as its execer and the in-machine
// entrypoint, and tells it the sidecar enters a sandbox that supplies its own
// HOME and PATH; the local backend is built exactly as it was before this
// node, spawned on the host at the configured entrypoint.
func claudeAdapter(kind workspace.Kind, opts runOptions, envMode claude.EnvMode, ws run.Workspace) *claude.Adapter {
	if kind == workspace.KindMachine {
		return claude.New(
			claude.WithEntrypoint(machine.SidecarNode, inMachineSidecar(opts.sidecar)),
			claude.WithEnvMode(envMode),
			claude.WithExecer(ws),
			claude.WithSandbox())
	}
	return claude.New(
		claude.WithEntrypoint(claude.DefaultCommand, opts.sidecar),
		claude.WithEnvMode(envMode))
}

// sidecarSourceDir is the built sidecar directory the worker materialises
// from: the parent of the entrypoint's directory, `<...>/sidecar` for the
// `<...>/sidecar/dist/main.js` the flag and homestead name. It holds `dist`
// and `node_modules`, which is the whole of what rides into a machine.
func sidecarSourceDir(sidecar string) string {
	return filepath.Dir(filepath.Dir(sidecar))
}

// inMachineSidecar is the entrypoint inside the machine: the materialised
// sidecar is bound at [machine.SidecarMount], so the in-machine path mirrors
// the host layout under it — `dist/main.js` for the standard build.
func inMachineSidecar(sidecar string) string {
	rel, err := filepath.Rel(sidecarSourceDir(sidecar), sidecar)
	if err != nil {
		rel = skill.SidecarEntrypoint
	}
	return filepath.Join(machine.SidecarMount, rel)
}

// workspacesFor is the selector: the backend the configuration named, as
// the function the run loop makes its workspace through. Local is what
// exists today and the default; machine is docs/sandbox.md's, chosen here
// and nowhere else, so that moving a worker from one to the other is the
// configuration change docs/sandbox.md §Self-hosting says it is.
func workspacesFor(kind workspace.Kind, opts runOptions, redactor *redact.Redactor) run.Workspaces {
	if kind == workspace.KindMachine {
		return machineWorkspaces(opts, redactor)
	}
	return localWorkspaces(opts, redactor)
}

// localWorkspaces makes a workspace under the worker's own account,
// reattaching one this worker left behind before cloning a new one. The
// run's repository goes in with it, because the `push` and `pr` verbs a
// session reaches over the socket are the workspace's and not the loop's.
func localWorkspaces(opts runOptions, redactor *redact.Redactor) run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec, repo run.Repository) (run.Workspace, error) {
		ws := local.New(local.Options{
			Root:     opts.workspaceDir,
			Origin:   repo.Origin,
			Forge:    repo.Forge,
			Name:     opts.name,
			Identity: opts.identity,
			Redactor: redactor,
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

// machineWorkspaces makes a workspace in a machine of the run's own,
// reattaching the machine this worker left behind before booting a new one.
// A run this host has a record of but no machine for is a lost sandbox,
// and that is the error: nothing boots a fresh machine for it
// (docs/sandbox.md §Reconciliation).
func machineWorkspaces(opts runOptions, redactor *redact.Redactor) run.Workspaces {
	return func(ctx context.Context, spec workspace.Spec, repo run.Repository) (run.Workspace, error) {
		ws := machine.New(machineDriver(), machineConfig(opts, repo, redactor))
		_, err := ws.Attach(ctx, spec)
		if err == nil {
			return ws, nil
		}
		if !errors.Is(err, machine.ErrNoRecord) {
			return nil, err
		}
		if _, err := ws.Create(ctx, spec); err != nil {
			return nil, err
		}
		return ws, nil
	}
}

// machineConfig is the machine backend's configuration as the flags and
// the file spell it.
func machineConfig(opts runOptions, repo run.Repository, redactor *redact.Redactor) machine.Config {
	sandbox := opts.sandbox
	return machine.Config{
		Root:     opts.sandboxRoot,
		Machines: sandbox.machinesDir,
		RunState: sandbox.runStateDir,
		Skill:    sandbox.skillDir,
		Sidecar:  sandbox.sidecarDir,
		// The workflow directory rides in beside them, when one was loaded
		// from, so a check step's scripts are in the machine
		// (docs/sandbox.md §"Skill mount").
		WorkflowSource: opts.workflowDir,
		Workflow:       sandbox.workflowCopyDir,
		Origin:         repo.Origin,
		Forge:          repo.Forge,
		Name:           opts.name,
		Identity:       opts.identity,
		// The account's one signing key, where the local backend keeps it
		// and `mill doctor` looks for it: a machine-backed run signs on
		// the host with the same key a local one does.
		SigningDir: filepath.Join(opts.workspaceDir, "signing"),
		Redactor:   redactor,
		Log:        os.Stderr,
		Images: image.Config{
			Registry:    sandbox.imageRegistry,
			Repository:  sandbox.imageRepository,
			Pin:         sandbox.baseImage,
			BuildBudget: sandbox.buildBudget,
		},
		Caches: cache.Config{
			GlobalCap:     sandbox.globalCap,
			GlobalCaps:    sandbox.globalCaps,
			RepoCap:       sandbox.repoCap,
			Expiry:        sandbox.cacheExpiry,
			HighWatermark: sandbox.highWatermark,
			LowWatermark:  sandbox.lowWatermark,
		},
		ImageExpiry:    sandbox.imageExpiry,
		CacheDriver:    cacheDriver(),
		Proxy:          sandbox.proxy,
		NoProxy:        sandbox.noProxy,
		NestedRepos:    sandbox.nestedRepos,
		DaggerCheckout: sandbox.daggerCheckout,
		ScratchBudget:  sandbox.scratchBudget,
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

// repositoryFor is where a run on the project delivers and what its
// workspace clones: the project's own `vcs_url`, as dagger carries it, and
// nothing the worker was configured with. A project with none is a
// *forge.NoVCSURLError. `--origin` overrides the clone URL and nothing else.
// The credential is the one the mill account holds, handed out per request
// by [forge.Static].
func repositoryFor(project *daggerv1.Project, opts runOptions) (run.Repository, error) {
	p := forge.Project{Slug: project.GetSlug(), VCSURL: project.GetVcsUrl()}
	f, err := forges().For(p, forge.Static(envOr(forgeUserEnv, defaultForgeUser), os.Getenv(forgeTokenEnv)))
	if err != nil {
		return run.Repository{}, err
	}
	origin := opts.origin
	if origin == "" {
		repo, err := forge.RepoFor(p)
		if err != nil {
			return run.Repository{}, err
		}
		origin = repo.CloneURL
	}
	return run.Repository{Forge: f, Origin: origin}, nil
}

// repositories is the seam a runner resolves each run's repository through.
func repositories(opts runOptions) run.Repositories {
	return func(_ context.Context, project *daggerv1.Project) (run.Repository, error) {
		return repositoryFor(project, opts)
	}
}

// projectRepository is repositoryFor for a caller with no claim in hand —
// the worker's startup check, the reconciliation sweep, `doctor`'s probe:
// one GetProject on the client's project, then the same rule.
func projectRepository(ctx context.Context, client *dagger.Client, opts runOptions) (run.Repository, error) {
	project, err := client.GetProject(ctx)
	if err != nil {
		return run.Repository{}, err
	}
	return repositoryFor(project, opts)
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
	case millv1.StepKind_STEP_KIND_PREPARATORY:
		return "preparatory"
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
