package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/ivy/mill/gen/mill/v1/millv1connect"
	"github.com/ivy/mill/internal/domain"
	"github.com/ivy/mill/internal/forge"
	"github.com/ivy/mill/internal/sdnotify"
	"github.com/ivy/mill/internal/server"
	"github.com/ivy/mill/internal/server/interceptor"
	"github.com/ivy/mill/internal/version"
	"github.com/ivy/mill/internal/workspace/socket"
)

// databaseURLEnv is the variable the service reads its connection string
// from when --database-url is not given. .env.example documents it.
const databaseURLEnv = "DATABASE_URL"

// readyProbe bounds the database ping the service reports readiness on.
const readyProbe = 5 * time.Second

// socketEnv names the socket every face of the binary reaches the service
// on when no --socket is given. A session started by the `plugin/mill`
// .mcp.json inherits it from the environment, which is the only channel a
// plugin has for it.
const socketEnv = "MILL_SOCKET"

// defaultSocketPath is $MILL_SOCKET, then the runtime directory of the
// invoking user when there is one, and the system runtime directory
// otherwise, which is where a systemd RuntimeDirectory= puts it.
func defaultSocketPath() string {
	if path := os.Getenv(socketEnv); path != "" {
		return path
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "mill", "mill.sock")
	}
	return "/run/mill/mill.sock"
}

type serveOptions struct {
	databaseURL     string
	roles           string
	shutdownTimeout time.Duration
	// work runs a worker in this same process. MVP runs `serve` and `work`
	// on one host as one unit (docs/architecture.md §"One binary, two
	// faces"); the Connect boundary between them is still there, so a
	// second worker elsewhere stays a configuration change.
	//
	// The socket the service listens on is the worker's --socket: one flag,
	// declared once by bindWorkFlags, because a worker in this process
	// reaches the store at the path this process binds.
	work   bool
	worker workOptions

	// watchRunsWindow and watchRunsKeepalive configure WatchRuns, the ledger's
	// change stream `mill status --follow` reads: how far back a resumed
	// cursor may be before it is refused, and how often an idle stream sends
	// a cursor-only keepalive so a proxy does not close it.
	watchRunsWindow    time.Duration
	watchRunsKeepalive time.Duration
}

// socket is the path the service listens on.
func (o *serveOptions) socket() string { return o.worker.run.socket }

// newServeCommand is the service face: Connect handlers over PostgreSQL,
// owning workflows, runs, the ledger, and step tokens.
func newServeCommand() *cobra.Command {
	opts := serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the service: the store, the workflows, and the ledger",
		Long: `Run the service: the mill.v1 StoreService over PostgreSQL, listening on a
unix socket. Migrations are applied at startup, before anything is answered.

A caller is identified by the unix account at the other end of the socket.
The account the service runs as does everything; other uids hold the role
--roles gives them, and a uid in no role reaches nothing.

Under a Type=notify unit the service reports itself ready once its socket is
bound and the database has answered. SIGTERM and SIGINT drain in-flight
requests before the process exits.

--work runs a worker in this same process, over this same socket, so one
unit is the whole mill. On shutdown the worker stops first and the store
drains after it, because the worker is one of the store's clients.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.roles, "roles", "", "path of the uid-to-role map (default: only the service's own uid)")
	f.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", server.DefaultShutdownTimeout, "how long to wait for in-flight requests on shutdown")
	f.BoolVar(&opts.work, "work", false, "also run a worker in this process, claiming from the configured projects")
	f.DurationVar(&opts.watchRunsWindow, "watch-runs-window", server.DefaultWatchRunsWindow, "how far back a WatchRuns cursor may be before it is refused")
	f.DurationVar(&opts.watchRunsKeepalive, "watch-runs-keepalive", server.DefaultWatchRunsKeepalive, "how often an idle WatchRuns stream sends a keepalive")
	bindWorkFlags(cmd, &opts.worker)
	return cmd
}

func runServe(cmd *cobra.Command, opts serveOptions) error {
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	if opts.work {
		// The file is read before anything is built, because --socket is
		// one of the settings it may carry and the listener is built from
		// it.
		if err := applyConfig(cmd, &opts.worker); err != nil {
			return err
		}
	}
	// Before the migrations, the pool and the bind: a socket path the
	// kernel cannot hold fails as "bind: invalid argument", and by then the
	// schema has been migrated for a service that will not start.
	if err := socket.CheckPath(opts.socket()); err != nil {
		return fmt.Errorf("mill serve: --socket: %w", err)
	}
	if opts.databaseURL == "" {
		return fmt.Errorf("mill serve: a database URL is required; pass --database-url or set %s", databaseURLEnv)
	}
	roles, err := interceptor.LoadRoleMap(opts.roles)
	if err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Migrations run before anything listens: the schema the handlers were
	// built against is the one they answer over, or the service does not
	// start. See the deployment section of docs/architecture.md.
	if err := domain.Migrate(ctx, opts.databaseURL); err != nil {
		return fmt.Errorf("mill serve: migrate: %w", err)
	}
	svc, err := domain.Open(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}
	defer svc.Close()

	// Every workflow under --workflows-root is pushed before anything is
	// reachable: a definition the loader refuses fails the start here,
	// loudly, rather than leaving the previous version silently in service
	// (docs/architecture.md §"Deployment"). Re-running this same idempotent
	// push at every start is what "push again when the digest changes"
	// means in practice, since PutWorkflow and PutSkill already store
	// nothing new for unchanged content.
	var hostMcpServers map[string]struct{}
	if opts.work {
		hostMcpServers = mcpServerNames(opts.worker.run.mcpServers)
	}
	if err := pushWorkflowsRoot(ctx, svc, opts.worker.run.workflowsRoot, hostMcpServers, log); err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}

	// WorkerService is mounted only when a worker actually runs in this
	// process: it answers the sidecar, signing and forge probes from this
	// process's own settings, and a bare `serve` has none of those to
	// answer from (docs/architecture.md §"One binary, two faces").
	var workerHandler millv1connect.WorkerServiceHandler
	if opts.work {
		identity := opts.worker.run.identity
		if identity == "" {
			identity = defaultIdentity()
		}
		// The projects Claims reports are the ones the worker actually
		// claims from, resolved the same way newWorker resolves them for the
		// worker itself (resolveProjects).
		projects := resolveProjects(opts.worker.projects)
		host, _ := os.Hostname()
		workerHandler = server.NewWorkerService(server.WorkerServiceConfig{
			SidecarEntrypoint: opts.worker.run.sidecar,
			EnvMode:           opts.worker.run.envMode,
			WorkspaceDir:      opts.worker.run.workspaceDir,
			Name:              opts.worker.run.name,
			Identity:          identity,
			Forge:             workerForgeResolver(&opts.worker),
			Host:              host,
			Projects:          projects,
			Labels:            opts.worker.labels,
		})
	}

	observer := server.NewObserver(server.DefaultObserverFallback)
	// PutWorkflow refuses a workflow whose mcp-servers collides with a name
	// this same process's worker already reserves — only meaningful when
	// --work colocates one (docs/architecture.md §"One binary, two faces");
	// a bare `serve` checks against nothing.
	storeOpts := []server.StoreServiceOption{
		server.WithWatchRunsWindow(opts.watchRunsWindow),
		server.WithWatchRunsKeepalive(opts.watchRunsKeepalive),
	}
	if opts.work {
		storeOpts = append(storeOpts, server.WithMcpServers(hostMcpServers))
	}
	// Rates are loaded whether or not a worker runs here: Status, Cost, and
	// Inspect price usage the same way from a bare `serve` as from `--work`
	// (docs/ledger.md §usage). Read directly rather than through
	// applyConfig, which only runs under --work and merges much more than
	// this one key.
	if err := checkNamedConfig(cmd, opts.worker.config); err != nil {
		return err
	}
	fileConfig, err := LoadConfig(opts.worker.config)
	if err != nil {
		return err
	}
	rateConfigs, err := loadRates(fileConfig.Rates, defaultRatesPath)
	if err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}
	rates := make(server.Rates, len(rateConfigs))
	for model, rate := range rateConfigs {
		rates[model] = server.Rate{Input: rate.Input, Output: rate.Output, CacheRead: rate.CacheRead, CacheWrite: rate.CacheWrite}
	}
	storeOpts = append(storeOpts, server.WithRates(rates))

	srv := server.New(server.Config{
		SocketPath:      opts.socket(),
		ShutdownTimeout: opts.shutdownTimeout,
		Logger:          log,
		Interceptors: []connect.Interceptor{
			interceptor.NewRoles(roles, server.Requirement()),
		},
	}, server.NewStoreService(svc, observer, opts.worker.run.logDir, storeOpts...), workerHandler)

	log.Info("mill serve starting", "version", version.String(), "socket", opts.socket())
	if err := srv.Listen(); err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}
	announceReady(ctx, svc, log)

	serveCtx, drain := context.WithCancel(context.WithoutCancel(ctx))
	defer drain()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx) }()

	// The worker is one of this store's clients, so on shutdown it goes
	// first and the handlers it writes through drain after it. `stopped`
	// is what says it has.
	worked := make(chan error, 1)
	stopped := make(chan struct{})
	if opts.work {
		worker, err := newWorker(cmd, &opts.worker, observer.Signal)
		if err != nil {
			return fmt.Errorf("mill serve: %w", err)
		}
		log.Info("worker starting in this process",
			"projects", opts.worker.projects, "labels", opts.worker.labels, "concurrency", opts.worker.concurrency)
		signals, stopSignalling := stopSignals()
		defer stopSignalling()
		go func() { worked <- supervise(ctx, worker, opts.worker.grace, signals) }()
	} else {
		worked <- nil
	}

	go func() {
		<-ctx.Done()
		log.Info("shutdown signal received")
		notifySystemd(log, sdnotify.Stopping)
		<-stopped
		drain()
	}()

	workErr := <-worked
	close(stopped)
	if workErr != nil {
		// The worker gave up on its own — dagger unreachable, most likely.
		// Serving on with no worker would be a mill that answers reads and
		// never takes work, so the process ends and the unit restarts it.
		drain()
	}
	if err := <-served; err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}
	return workErr
}

// announceReady reports the service ready to systemd once the database
// answers, and not before: the socket is already bound, but a service that
// cannot reach its database cannot take work, and a unit ordered after this
// one should not start until it can.
func announceReady(ctx context.Context, svc *domain.Service, log *slog.Logger) {
	probeCtx, cancel := context.WithTimeout(ctx, readyProbe)
	defer cancel()
	if err := svc.Ping(probeCtx); err != nil {
		log.Warn("database not reachable; not reporting ready", "err", err)
		return
	}
	log.Info("database reachable; ready")
	notifySystemd(log, sdnotify.Ready, sdnotify.Status("serving"))
}

// notifySystemd sends states to systemd, logging a failure to reach the
// notification socket rather than failing the service over it.
func notifySystemd(log *slog.Logger, states ...string) {
	if _, err := sdnotify.Send(states...); err != nil {
		log.Warn("notify systemd", "err", err)
	}
}

// mcpServerNames is the set of a worker's configured mcp-servers, for
// PutWorkflow's collision check, which cares which names are taken and
// nothing else about them.
func mcpServerNames(configured map[string]McpServerConfig) map[string]struct{} {
	if len(configured) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(configured))
	for name := range configured {
		names[name] = struct{}{}
	}
	return names
}

// workerForgeResolver is WorkerServiceConfig's Forge, over --work's own
// settings: the first configured project, the way newWorker picks one for
// workerStartup, since the RPC answers for this process's one worker and
// not a project named per call. Resolved fresh on every probe — through
// resolveForgeProject, the same resolution probeForge and
// signingContentReader use — so a rotated credential is the one reported.
func workerForgeResolver(opts *workOptions) func(context.Context) (forge.Forge, string, string, error) {
	projects := resolveProjects(opts.projects)
	return func(ctx context.Context) (forge.Forge, string, string, error) {
		if len(projects) == 0 {
			return nil, "", "", fmt.Errorf("no project to read a repository from: pass --project or set %s", daggerProjectEnv)
		}
		token, err := loadForgeToken()
		if err != nil {
			return nil, "", "", err
		}
		if token.value == "" {
			return nil, "", "", fmt.Errorf("the mill holds no forge credential: deliver %s to the credential store or set $%s",
				forgeCredentialName, forgeTokenEnv)
		}
		f, repositoryURL, err := resolveForgeProject(ctx, opts.run.daggerSocket, projects[0], opts.run.forge)
		if err != nil {
			return nil, "", "", err
		}
		return f, token.source, repositoryURL, nil
	}
}
