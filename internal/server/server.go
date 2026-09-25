// Package server is the service face of the binary: the mill.v1 Connect
// handlers on an HTTP server listening on a unix socket.
//
// The package owns the transport and nothing else. The service
// implementation is injected, the handlers never reach the store — only
// [github.com/steading-ai/mill/internal/domain] may, and depguard enforces it — and
// a caller is whatever the connection says it is. In O0 the only caller is
// the worker face of this same process, and it speaks Connect over the
// socket like anything else: the boundary exists from the first commit so
// that a worker on another host is a configuration change. See
// docs/architecture.md §"One binary, two faces".
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/steading-ai/mill/gen/mill/v1/millv1connect"
	"github.com/steading-ai/mill/internal/server/interceptor"
)

// Defaults for the fields of Config that may be left zero.
const (
	// DefaultSocketMode admits the owner and the group and nobody else.
	DefaultSocketMode = os.FileMode(0o660)
	// DefaultShutdownTimeout bounds the drain on SIGTERM.
	DefaultShutdownTimeout = 30 * time.Second
)

// Config is everything the transport needs to know.
type Config struct {
	// SocketPath is where the unix socket is created. Its parent directory
	// is created when missing. A stale socket nobody listens on is
	// replaced; a live one is an error.
	SocketPath string
	// SocketMode is the permission set on the socket. Zero means
	// [DefaultSocketMode].
	SocketMode os.FileMode
	// ShutdownTimeout bounds the drain. Zero means
	// [DefaultShutdownTimeout].
	ShutdownTimeout time.Duration
	// Logger receives the server's structured log. Nil means slog.Default.
	Logger *slog.Logger
	// Interceptors wrap every handler, in order. Role resolution is the
	// first of them; see [interceptor.NewRoles].
	Interceptors []connect.Interceptor
}

// Server is the store's handlers on one mux, served over the socket.
type Server struct {
	cfg      Config
	log      *slog.Logger
	mux      *http.ServeMux
	http     *http.Server
	listener net.Listener
}

// New builds a server over store. A nil store is mounted as unimplemented,
// which answers every call with a Connect Unimplemented error rather than a
// 404.
//
// worker is optional: a nil worker mounts no route for WorkerService at all,
// so a caller against a mill serve with no worker in it gets a plain 404,
// which connect-go's client turns into the same CodeUnimplemented a mounted
// UnimplementedWorkerServiceHandler would answer with. Callers that need to
// tell "no worker here" apart from "worker refused" do not need to: both are
// this code.
func New(cfg Config, store millv1connect.StoreServiceHandler, worker millv1connect.WorkerServiceHandler) *Server {
	if cfg.SocketMode == 0 {
		cfg.SocketMode = DefaultSocketMode
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if store == nil {
		store = millv1connect.UnimplementedStoreServiceHandler{}
	}

	s := &Server{cfg: cfg, log: cfg.Logger, mux: http.NewServeMux()}
	s.mux.Handle(millv1connect.NewStoreServiceHandler(store,
		connect.WithInterceptors(cfg.Interceptors...)))
	if worker != nil {
		s.mux.Handle(millv1connect.NewWorkerServiceHandler(worker,
			connect.WithInterceptors(cfg.Interceptors...)))
	}
	s.http = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Every connection over the socket carries its peer's unix
		// credentials, which is what the role interceptor reads.
		ConnContext: interceptor.UnixConnContext,
	}
	return s
}

// Handler is the mounted mux, for serving over some other transport.
func (s *Server) Handler() http.Handler { return s.mux }

// Listen binds the socket and applies its permissions. It returns once the
// listener accepts connections, so a caller that needs to know the server
// is up waits on this rather than polling the path.
func (s *Server) Listen() error {
	if s.listener != nil {
		return errors.New("server: already listening")
	}
	l, err := listenUnix(s.cfg)
	if err != nil {
		return err
	}
	s.listener = l
	s.log.Info("listening", "socket", s.cfg.SocketPath, "mode", fmt.Sprintf("%#o", s.cfg.SocketMode))
	return nil
}

// Serve answers requests until ctx is cancelled, then drains in-flight ones
// within the shutdown timeout and returns. It calls [Server.Listen] when
// the caller has not.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	go func() {
		err := s.http.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	s.log.Info("draining", "timeout", s.cfg.ShutdownTimeout)
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// The drain ran out of time. Close what is left rather than
		// leaving connections and the socket behind.
		return errors.Join(fmt.Errorf("server: drain: %w", err), s.http.Close(), <-done)
	}
	return <-done
}

// Close stops serving immediately, without a drain. Serve's return is the
// ordinary path; this is for a caller that never started one.
func (s *Server) Close() error {
	err := s.http.Close()
	s.listener = nil
	return err
}
