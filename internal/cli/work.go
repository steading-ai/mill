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

	"github.com/ivy/mill/internal/harness/claude"
	"github.com/ivy/mill/internal/run"
)

// workOptions is what the daemon needs on top of a run's own settings.
type workOptions struct {
	run         runOptions
	config      string
	projects    []string
	labels      []string
	concurrency int
	wait        time.Duration
	grace       time.Duration
}

// newWorkCommand is the worker face: ClaimNext against dagger, then drive
// the claimed node's run.
func newWorkCommand() *cobra.Command {
	opts := workOptions{}
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Claim ready dagger nodes and drive their runs",
		Long: `Run a worker: ask dagger for the next ready node matching the configured
projects and labels, drive its run to a result, and ask again. There is no
scheduler and no event subscription — readiness is dagger's, and a worker
asking for work is the scheduling.

--concurrency caps how many runs are in flight; one is the value this
release is built around. The filters are the whole of the selection a worker
makes: a node outside them is never claimed.

SIGTERM stops the worker: it claims nothing more, lets the run in flight
reach a boundary, releases its lease as a preemption with detail "shutdown",
and leaves the run resumable at that step. A second SIGTERM inside the grace
window kills the sessions and exits non-zero.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWork(cmd, &opts)
		},
	}
	bindWorkFlags(cmd, &opts)
	return cmd
}

// bindWorkFlags declares the worker's flags on a command. `serve --work`
// takes the same set, so the one process runs the same worker the standalone
// command does.
func bindWorkFlags(cmd *cobra.Command, opts *workOptions) {
	f := cmd.Flags()
	f.StringVar(&opts.config, "config", defaultConfigPath(), "path of the configuration file (default: $"+configEnv+", else $XDG_CONFIG_HOME/mill/config.yaml)")
	f.StringSliceVar(&opts.projects, "project", nil, "dagger project to claim from; repeat for more (default: $"+daggerProjectEnv+")")
	f.StringSliceVar(&opts.labels, "label", nil, "only claim nodes carrying every one of these labels; repeat for more")
	f.IntVar(&opts.concurrency, "concurrency", run.DefaultConcurrency, "how many runs to drive at once")
	f.DurationVar(&opts.wait, "wait", run.DefaultWait, "how long one claim blocks for work before asking again")
	f.DurationVar(&opts.grace, "grace", run.DefaultStopGrace, "how long a stopping worker gives its runs before a second signal kills them")
	f.StringVar(&opts.run.daggerSocket, "dagger-socket", envOr(daggerSocketEnv, defaultDaggerSocket), "path of dagger's unix socket (default: $"+daggerSocketEnv+")")
	f.StringVar(&opts.run.workflow, "workflow", "", "workflow to select when a run has none (default: assess the node and let the assessment name one)")
	f.StringVar(&opts.run.tier, "tier", "", "tier that workflow stands for, which gate policies compare against (default: quick-fix, or the assessed tier)")
	f.StringVar(&opts.run.workflowDir, "workflow-dir", "", "directory the workflow was loaded from, for a check step's scripts")
	f.StringVar(&opts.run.sidecar, "sidecar", envOr(sidecarEnv, claude.DefaultEntrypoint), "path of the harness sidecar's entrypoint (default: $"+sidecarEnv+")")
	f.StringVar(&opts.run.vcsURL, "vcs-url", os.Getenv(vcsURLEnv), "repository the projects deliver to (default: $"+vcsURLEnv+")")
	f.StringVar(&opts.run.origin, "origin", "", "repository a new workspace clones (default: the --vcs-url repository)")
	f.StringVar(&opts.run.ref, "ref", "", "ref a new workspace checks out (default: the repository's own HEAD)")
	f.StringVar(&opts.run.workspaceDir, "workspace-dir", defaultWorkspaceDir(), "directory run workspaces are made under")
	f.StringVar(&opts.run.logDir, "log-dir", defaultLogDir(), "directory step logs are written under, one file per attempt")
	f.StringVar(&opts.run.envMode, "env-mode", string(claude.DefaultEnvMode), envModeUsage)
	f.StringVar(&opts.run.notifyCommand, "notify-command", "", notifyCommandUsage)
	f.StringVar(&opts.run.identity, "identity", "", "address the mill commits under (default: mill@<host>)")
	f.StringVar(&opts.run.name, "name", defaultForgeUser, "name on the mill's commits")
	f.DurationVar(&opts.run.ttl, "ttl", run.DefaultTTL, "lease TTL to ask dagger for")
	f.DurationVar(&opts.run.interval, "heartbeat", 0, "how often to renew the lease (default: a third of the TTL)")
}

// applyConfig fills in every worker flag the command line left alone. The
// socket flag is not declared here — `work` and `serve` each declare their
// own — so it is passed in.
func applyConfig(cmd *cobra.Command, opts *workOptions, socket *string) error {
	if err := checkNamedConfig(cmd, opts.config); err != nil {
		return err
	}
	config, err := LoadConfig(opts.config)
	if err != nil {
		return err
	}
	targets := map[string]*string{
		"socket": socket, "dagger-socket": &opts.run.daggerSocket,
		"workflow": &opts.run.workflow, "tier": &opts.run.tier,
		"workflow-dir": &opts.run.workflowDir, "sidecar": &opts.run.sidecar,
		"vcs-url": &opts.run.vcsURL, "origin": &opts.run.origin, "ref": &opts.run.ref,
		"identity": &opts.run.identity, "name": &opts.run.name,
		"workspace-dir": &opts.run.workspaceDir, "log-dir": &opts.run.logDir,
		"env-mode": &opts.run.envMode, "notify-command": &opts.run.notifyCommand,
	}
	values := map[string]string{
		"socket": config.Socket, "dagger-socket": config.DaggerSocket,
		"workflow": config.Workflow, "tier": config.Tier,
		"workflow-dir": config.WorkflowDir, "sidecar": config.Sidecar,
		"vcs-url": config.VCSURL, "origin": config.Origin, "ref": config.Ref,
		"identity": config.Identity, "name": config.Name,
		"workspace-dir": config.WorkspaceDir, "log-dir": config.LogDir,
		"env-mode": config.EnvMode, "notify-command": config.NotifyCommand,
	}
	for name, target := range targets {
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
	return nil
}

// runWork builds the worker and supervises it.
func runWork(cmd *cobra.Command, opts *workOptions) error {
	if err := applyConfig(cmd, opts, &opts.run.socket); err != nil {
		return err
	}
	worker, err := newWorker(cmd, opts)
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
func newWorker(cmd *cobra.Command, opts *workOptions) (*run.Worker, error) {
	projects := opts.projects
	if len(projects) == 0 {
		if fallback := os.Getenv(daggerProjectEnv); fallback != "" {
			projects = []string{fallback}
		}
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("mill work: at least one dagger project is required; pass --project or set %s", daggerProjectEnv)
	}
	if opts.run.socket == "" {
		opts.run.socket = defaultSocketPath()
	}

	runners := make([]*run.Runner, 0, len(projects))
	for _, project := range projects {
		// No answerer: a worker has nobody at its terminal, so a gate is
		// filed and the lease released, and whoever answers does it in
		// dagger (docs/architecture.md §"Gate steps").
		runner, err := newRunner(cmd, opts.run, project, "", nil)
		if err != nil {
			return nil, err
		}
		runners = append(runners, runner)
	}
	return run.NewWorker(run.WorkerOptions{
		Runners:     runners,
		Labels:      opts.labels,
		Concurrency: opts.concurrency,
		Wait:        opts.wait,
		Out:         cmd.OutOrStdout(),
		Log:         slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
}

// stopper is a worker as its supervisor sees it: something that runs until
// it is asked to stop, and something that can be asked.
type stopper interface {
	Run(ctx context.Context) error
	Stop()
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
		case <-done:
			return
		}
		worker.Stop()
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
