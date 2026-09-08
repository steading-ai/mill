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
requests before the process exits.`,
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
	return cmd
}

func runServe(cmd *cobra.Command, opts serveOptions) error {
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))
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
	go func() {
		<-ctx.Done()
		log.Info("shutdown signal received")
		notify(log, sdnotify.Stopping)
		drain()
	}()
	if err := srv.Serve(serveCtx); err != nil {
		return fmt.Errorf("mill serve: %w", err)
	}
	return nil
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
	notify(log, sdnotify.Ready, sdnotify.Status("serving"))
}

// notify sends states to systemd, logging a failure to reach the
// notification socket rather than failing the service over it.
func notify(log *slog.Logger, states ...string) {
	if _, err := sdnotify.Send(states...); err != nil {
		log.Warn("notify systemd", "err", err)
	}
}
