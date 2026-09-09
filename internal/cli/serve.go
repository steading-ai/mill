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

	"github.com/ivy/mill/internal/domain"
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

// defaultSocketPath is the runtime directory of the invoking user when
// there is one, and the system runtime directory otherwise, which is where a
// systemd RuntimeDirectory= puts it.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "mill", "mill.sock")
	}
	return "/run/mill/mill.sock"
}

type serveOptions struct {
	socket          string
	databaseURL     string
	roles           string
	shutdownTimeout time.Duration
	// work runs a worker in this same process. MVP runs `serve` and `work`
	// on one host as one unit (docs/architecture.md §"One binary, two
	// faces"); the Connect boundary between them is still there, so a
	// second worker elsewhere stays a configuration change.
	work   bool
	worker workOptions
}

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
	f.StringVar(&opts.socket, "socket", defaultSocketPath(), "path of the unix socket to listen on")
	f.StringVar(&opts.databaseURL, "database-url", os.Getenv(databaseURLEnv), "PostgreSQL connection URL (default: $"+databaseURLEnv+")")
	f.StringVar(&opts.roles, "roles", "", "path of the uid-to-role map (default: only the service's own uid)")
	f.DurationVar(&opts.shutdownTimeout, "shutdown-timeout", server.DefaultShutdownTimeout, "how long to wait for in-flight requests on shutdown")
	f.BoolVar(&opts.work, "work", false, "also run a worker in this process, claiming from the configured projects")
	bindWorkFlags(cmd, &opts.worker)
	return cmd
}

func runServe(cmd *cobra.Command, opts serveOptions) error {
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	if opts.work {
		// The file is read before anything is built, because --socket is
		// one of the settings it may carry and the listener is built from
		// it.
		if err := applyConfig(cmd, &opts.worker, &opts.socket); err != nil {
			return err
		}
	}
	// Before the migrations, the pool and the bind: a socket path the
	// kernel cannot hold fails as "bind: invalid argument", and by then the
	// schema has been migrated for a service that will not start.
	if err := socket.CheckPath(opts.socket); err != nil {
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

	srv := server.New(server.Config{
		SocketPath:      opts.socket,
		ShutdownTimeout: opts.shutdownTimeout,
		Logger:          log,
		Interceptors: []connect.Interceptor{
			interceptor.NewRoles(roles, server.Requirement()),
		},
	}, server.NewStoreService(svc))

	log.Info("mill serve starting", "version", version.String(), "socket", opts.socket)
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
		opts.worker.run.socket = opts.socket
		worker, err := newWorker(cmd, &opts.worker)
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
