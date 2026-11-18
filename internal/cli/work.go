package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steading-ai/mill/internal/forge"
	"github.com/steading-ai/mill/internal/harness/claude"
	"github.com/steading-ai/mill/internal/run"
	"github.com/steading-ai/mill/internal/workspace"
	"github.com/steading-ai/mill/internal/workspace/machine"
)

// workOptions is what the daemon needs on top of a run's own settings.
type workOptions struct {
	run           runOptions
	config        string
	projects      []string
	labels        []string
	concurrency   int
	wait          time.Duration
	sweepInterval time.Duration
	grace         time.Duration
}

// newWorkCommand is the worker face: ClaimNext against skift, then drive
// the claimed node's run.
func newWorkCommand() *cobra.Command {
	return workCommand(&workOptions{})
}

// workCommand is the worker command over the options it fills, so a spec
// can read what the command resolved.
func workCommand(opts *workOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Claim ready skift nodes and drive their runs",
		Long: `Run a worker: ask skift for the next ready node matching the configured
projects and labels, drive its run to a result, and ask again. With no
--project it claims from every project the worker's principal may read.
There is no scheduler and no event subscription — readiness is skift's, and
a worker asking for work is the scheduling.

--concurrency caps how many runs are in flight; one is the value this
release is built around. The filters are the whole of the selection a worker
makes: a node outside them is never claimed.

SIGTERM asks the worker to drain: it claims nothing more, and every run in
flight finishes its current attempt and stops at its next step boundary,
its lease released as a preemption with detail "drain", resumable there
with no attempt aborted. A second SIGTERM inside the grace window kills
whatever is still in flight, releases with detail "shutdown", and exits
non-zero.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWork(cmd, opts)
		},
	}
	bindWorkFlags(cmd, opts)
	return cmd
}

// bindWorkFlags declares the worker's flags on a command. `serve --work`
// takes the same set, so the one process runs the same worker the standalone
// command does.
func bindWorkFlags(cmd *cobra.Command, opts *workOptions) {
	f := cmd.Flags()
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml, else /etc/mill/config.yaml)")
	f.StringVar(&opts.run.socket, "socket", defaultSocketPath(), "path of the service's unix socket: what `serve` listens on, and what a worker reaches it at (default: $"+socketEnv+")")
	f.StringSliceVar(&opts.projects, "project", nil, "skift project to claim from; repeat for more (default: $"+skiftProjectEnv+", else every project the worker may read)")
	f.StringSliceVar(&opts.labels, "label", nil, "only claim nodes carrying every one of these labels; repeat for more")
	f.IntVar(&opts.concurrency, "concurrency", run.DefaultConcurrency, "how many runs to drive at once")
	f.DurationVar(&opts.wait, "wait", run.DefaultWait, "how long one claim blocks for work before asking again")
	f.DurationVar(&opts.sweepInterval, "sweep-interval", run.DefaultSweepInterval, "how long the worker goes between sweeps of the ledger against skift, for runs whose node was closed underneath them")
	f.DurationVar(&opts.grace, "grace", run.DefaultStopGrace, "how long a stopping worker gives its runs before a second signal kills them")
	f.StringVar(&opts.run.skiftSocket, "skift-socket", envOr(skiftSocketEnv, defaultSkiftSocket), "path of skift's unix socket (default: $"+skiftSocketEnv+")")
	f.StringVar(&opts.run.workflow, "workflow", "", "workflow to select when a run has none (default: assess the node and let the assessment name one)")
	f.StringVar(&opts.run.tier, "tier", "", "tier that workflow stands for, which gate policies compare against (default: quick-fix, or the assessed tier)")
	f.StringVar(&opts.run.workflowDir, "workflow-dir", "", "directory the workflow was loaded from, for a check step's scripts")
	f.StringVar(&opts.run.workflowsRoot, "workflows-root", "", "directory of workflow subdirectories `serve` pushes to the store at every start (default: none, meaning nothing is pushed)")
	f.StringVar(&opts.run.sidecar, "sidecar", envOr(sidecarEnv, claude.DefaultEntrypoint), "path of the harness sidecar's entrypoint (default: $"+sidecarEnv+")")
	f.StringVar(&opts.run.origin, "origin", "", "repository a new workspace clones (default: the project's repository)")
	f.StringVar(&opts.run.ref, "ref", "", "ref a new workspace checks out (default: the repository's own HEAD)")
	f.StringVar(&opts.run.forge, "forge", "", forgeUsage)
	f.StringVar(&opts.run.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces are made under")
	f.StringVar(&opts.run.workspace, "workspace", string(workspace.KindLocal), workspaceUsage)
	f.StringVar(&opts.run.sandboxRoot, "sandbox-root", machine.DefaultRoot, sandboxRootUsage)
	f.StringVar(&opts.run.logDir, "log-dir", defaultLogDir(), "directory step logs are written under, one file per attempt")
	f.StringVar(&opts.run.envMode, "env-mode", string(claude.DefaultEnvMode), envModeUsage)
	f.StringVar(&opts.run.notifyCommand, "notify-command", "", notifyCommandUsage)
	f.StringVar(&opts.run.identity, "identity", "", "address the mill commits under (default: mill@<host>)")
	f.StringVar(&opts.run.name, "name", defaultForgeUser, "name on the mill's commits")
	f.DurationVar(&opts.run.ttl, "ttl", run.DefaultTTL, "lease TTL to ask skift for")
	f.DurationVar(&opts.run.interval, "heartbeat", 0, "how often to renew the lease (default: a third of the TTL)")
	bindGatewayFlags(cmd, &opts.run.gateway)
}

// applyConfig fills in every worker flag the command line left alone. Every
// key it reads has a flag bindWorkFlags declared on the same command — or
// is one of the machine backend's, which has no flag and is the file's
// alone. configKeys is the list, and docs/tools.md §Configuration is the
// same list as an operator reads it. The sockets go through the resolver
// every face shares (sockets.go), so `work` and `serve --work` reach the
// service the way `status` does: the flag, then the variable, then the
// file.
func applyConfig(cmd *cobra.Command, opts *workOptions) error {
	if err := checkNamedConfig(cmd, opts.config); err != nil {
		return err
	}
	config, err := LoadConfig(opts.config)
	if err != nil {
		return err
	}
	if err := resolveSockets(cmd, config); err != nil {
		return err
	}
	values := stringValues(config)
	for name, target := range stringTargets(opts) {
		if value := values[name]; value != "" && unset(cmd, name) {
			*target = value
		}
	}
	if len(config.Projects) > 0 && unset(cmd, "project") {
		opts.projects = config.Projects
	}
	if len(config.Labels) > 0 && unset(cmd, "label") {
		opts.labels = config.Labels
	}
	if config.Concurrency > 0 && unset(cmd, "concurrency") {
		opts.concurrency = config.Concurrency
	}
	for name, target := range map[string]*time.Duration{"ttl": &opts.run.ttl, "heartbeat": &opts.run.interval} {
		value := map[string]string{"ttl": config.TTL, "heartbeat": config.Heartbeat}[name]
		if value == "" || !unset(cmd, name) {
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("mill: %s: %s: %w", opts.config, name, err)
		}
		*target = d
	}
	if config.Gateway && unset(cmd, "gateway") {
		opts.run.gateway.enabled = true
	}
	if len(config.McpServers) > 0 {
		opts.run.mcpServers = config.McpServers
	}
	if config.Retention != "" {
		opts.run.retention = config.Retention
	}
	if len(config.RetentionProjects) > 0 {
		opts.run.retentionProjects = config.RetentionProjects
	}
	if err := resolveSandboxRoot(cmd, &opts.run.sandboxRoot, config.SandboxRoot); err != nil {
		return err
	}
	return applySandboxConfig(config, &opts.run.sandbox)
}

// stringTargets is where each string-valued flag's setting lives, by flag
// name; stringValues is what the file says for each. The two are keyed the
// same so a key added to one without the other is a spec failure, not a
// silently dropped setting. The sockets are not here: resolveSockets owns
// them, because the environment outranks the file for those alone.
func stringTargets(opts *workOptions) map[string]*string {
	return map[string]*string{
		"workflow": &opts.run.workflow, "tier": &opts.run.tier,
		"workflow-dir": &opts.run.workflowDir, "workflows-root": &opts.run.workflowsRoot, "sidecar": &opts.run.sidecar,
		"origin": &opts.run.origin, "ref": &opts.run.ref, "forge": &opts.run.forge,
		"identity": &opts.run.identity, "name": &opts.run.name,
		"workspace-dir": &opts.run.workspaceDir, "log-dir": &opts.run.logDir,
		"workspace": &opts.run.workspace, "sandbox-root": &opts.run.sandboxRoot,
		"env-mode": &opts.run.envMode, "notify-command": &opts.run.notifyCommand,
		"gateway-listen": &opts.run.gateway.listen, "gateway-upstream": &opts.run.gateway.upstream,
		"model-credential-file": &opts.run.gateway.credentialFile,
	}
}

func stringValues(config Config) map[string]string {
	return map[string]string{
		"workflow": config.Workflow, "tier": config.Tier,
		"workflow-dir": config.WorkflowDir, "workflows-root": config.WorkflowsRoot, "sidecar": config.Sidecar,
		"origin": config.Origin, "ref": config.Ref, "forge": config.Forge,
		"identity": config.Identity, "name": config.Name,
		"workspace-dir": config.WorkspaceDir, "log-dir": config.LogDir,
		"workspace": config.Workspace, "sandbox-root": config.SandboxRoot,
		"env-mode": config.EnvMode, "notify-command": config.NotifyCommand,
		"gateway-listen": config.GatewayListen, "gateway-upstream": config.GatewayUpstream,
		"model-credential-file": config.ModelCredentialFile,
	}
}

// runWork builds the worker and supervises it.
func runWork(cmd *cobra.Command, opts *workOptions) error {
	if err := applyConfig(cmd, opts); err != nil {
		return err
	}
	worker, err := newWorker(cmd, opts, nil)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("mill work starting", "projects", opts.projects, "labels", opts.labels, "concurrency", opts.concurrency)
	signals, stop := stopSignals()
	defer stop()
	return supervise(cmd.Context(), worker, opts.grace, signals)
}

// stopSignals is the process's stop signals as a channel. It is a channel
// rather than a [signal.NotifyContext] because the supervisor's whole job is
// telling the first from the second, and a context can only be cancelled
// once. The buffer holds both, so neither is dropped while the first is
// being acted on.
func stopSignals() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	return signals, func() { signal.Stop(signals) }
}

// newWorker assembles one runner per project and the loop over them.
// onActivity, when not nil, is wired into every runner's [run.Options] so
// its attempts' activity writers reach a same-process observer
// (`serve --work`); nil is a worker with no co-located server to notify.
func newWorker(cmd *cobra.Command, opts *workOptions, onActivity func(string)) (*run.Worker, error) {
	projects := resolveProjects(opts.projects)
	if opts.run.socket == "" {
		opts.run.socket = defaultSocketPath()
	}
	// A misspelt `forge:` is refused here, once, rather than at every claim:
	// the value is the worker's own, the same whichever project a claim
	// lands in, so it needs no project to check against (unlike the
	// repository itself, which the plan resolves fresh per claim).
	if _, err := forge.ParseKind(opts.run.forge); err != nil {
		return nil, fmt.Errorf("mill work: --forge: %w", err)
	}
	// The scratch namespace lives on one project's forge (workerStartup
	// below), so it needs a single project to resolve; an empty or a
	// many-project filter has no one project to ask.
	if opts.run.sandbox.scratchNamespace != "" && len(projects) != 1 {
		return nil, fmt.Errorf("mill work: --scratch-namespace requires exactly one --project")
	}

	// One gateway for the process, in front of every project's runs. It
	// outlives the worker: the listener is the process's, closed with it.
	gw, err := startGateway(cmd, opts.run.gateway)
	if err != nil {
		return nil, err
	}
	// One client for the worker, whatever the filter: an empty or
	// many-project filter has no one project to pin the client to, and
	// every claim scopes its own writes to the project it actually landed
	// in (internal/run/run.go's state.skift). No answerer: a worker has
	// nobody at its terminal, so a gate is filed and the lease released,
	// and whoever answers does it in skift (docs/architecture.md §"Gate
	// steps").
	project := ""
	if len(projects) == 1 {
		project = projects[0]
	}
	runner, err := newRunner(cmd, opts.run, project, "", nil, gw, onActivity, opts.concurrency)
	if err != nil {
		return nil, err
	}
	// What precedes the first claim: the machine backend's reconciliation
	// of the host against the ledger, then eviction (docs/sandbox.md
	// §Reconciliation).
	kind, err := workspace.ParseKind(opts.run.workspace)
	if err != nil {
		return nil, fmt.Errorf("mill work: --workspace: %w", err)
	}
	worker, err := workerRef()
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	startup, err := workerStartup(cmd.Context(), kind, opts.run, project, worker, cmd.OutOrStdout(), log)
	if err != nil {
		return nil, err
	}
	return run.NewWorker(run.WorkerOptions{
		Runner:        runner,
		Projects:      projects,
		Startup:       startup,
		Labels:        opts.labels,
		Concurrency:   opts.concurrency,
		Wait:          opts.wait,
		SweepInterval: opts.sweepInterval,
		Out:           cmd.OutOrStdout(),
		Log:           log,
	})
}

// stopper is a worker as its supervisor sees it: something that runs until
// it is asked to stop, and something that can be asked.
type stopper interface {
	Run(ctx context.Context) error
	Drain()
}

// supervise turns signals into the two stops docs/architecture.md §"Failure
// and stop" names.
//
// The first SIGTERM is the graceful one: stop claiming, let the run in
// flight reach a boundary, release its lease as a preemption, leave the
// workspace intact. The second — or the grace window elapsing, which is the
// same thing said by a clock rather than by an operator — kills the sessions
// and makes the exit non-zero.
//
// A caller with a context of its own (`serve --work`) still gets the
// graceful stop, because a cancelled context asks for it too.
func supervise(ctx context.Context, worker stopper, grace time.Duration, signals <-chan os.Signal) error {
	// The worker's own context is never the one that is cancelled by a
	// signal: a stop is a request the run answers by leaving itself
	// resumable, and a cancelled context would instead cut its writes off
	// halfway. Only the kill cancels it.
	killable, kill := context.WithCancel(context.WithoutCancel(ctx))
	defer kill()

	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
		case <-ctx.Done():
			// A caller whose ctx is itself signal-derived delivers one
			// SIGTERM to both destinations: ctx is cancelled and the same
			// signal lands on the channel, dispatched back to back by the
			// same os/signal loop. Drop a copy already queued from that
			// delivery so the second select below does not read it as an
			// unrelated second stop.
			select {
			case <-signals:
			default:
			}
		case <-done:
			return
		}
		worker.Drain()
		select {
		case <-signals:
		case <-time.After(grace):
		case <-done:
			return
		}
		kill()
	}()

	err := worker.Run(killable)
	close(done)
	return err
}
