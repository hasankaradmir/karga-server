// Package server runs karga-server: it owns the state database and the container runtime and
// serves the release operations to the karga CLI on a unix socket.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/logger"

	"github.com/hasankaradmir/karga-server/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/reconciler"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/config"
	"github.com/hasankaradmir/karga-server/internal/ports"
	"github.com/hasankaradmir/karga-server/internal/version"
)

// Config is how karga-server runs.
type Config struct {
	Home   string
	Socket string
	// SocketMode is the socket's permission (default 0660: the server's user and group).
	SocketMode os.FileMode
	Log        *slog.Logger
	// ReconcileInterval is the time between full reconciliation passes (0: events only); a
	// negative value turns reconciliation off (tests).
	ReconcileInterval time.Duration
}

// DefaultReconcileInterval is how often every release is looked at besides runtime events.
const DefaultReconcileInterval = 30 * time.Second

var _ api.Backend = (*releases.Service)(nil)

// status maps karga-server's errors to HTTP status codes; the CLI shows only their messages.
func status(err error) int {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, releases.ErrLocked), errors.Is(err, releases.ErrInterrupted), errors.Is(err, releases.ErrExists),
		errors.Is(err, ports.ErrPortInUse), errors.Is(err, ports.ErrConflict):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func init() { api.SetStatus(status) }

// Server is a running karga-server.
type Server struct {
	cfg     Config
	Service *releases.Service
	Loop    *reconciler.Loop
	store   *sqlite.Store
	unlock  func()
}

// New opens the state in cfg.Home (taking it for this server alone) and prepares the service on rt.
func New(ctx context.Context, cfg Config, rt ports.Runtime) (*Server, error) {
	if cfg.SocketMode == 0 {
		cfg.SocketMode = 0o660
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if err := config.EnsureHome(config.Config{Home: cfg.Home}); err != nil {
		return nil, err
	}
	unlock, err := lockHome(cfg.Home)
	if err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, filepath.Join(cfg.Home, "karga.db"))
	if err != nil {
		unlock()
		return nil, err
	}
	redactor := logger.NewRedactor()
	svc := releases.New(store, func(context.Context) (ports.Runtime, error) { return rt, nil }, cfg.Home, redactor.Add, cfg.Log)
	info, err := rt.Info(ctx)
	if err != nil {
		_ = store.Close()
		unlock()
		return nil, err
	}
	deployment.UseProxies(info)
	s := &Server{cfg: cfg, Service: svc, store: store, unlock: unlock}
	if cfg.ReconcileInterval >= 0 {
		s.Loop = reconciler.New(svc, cfg.ReconcileInterval, cfg.Log)
		svc.AfterChange = s.Loop.Notify
	}
	return s, nil
}

// lockHome makes sure only one server uses a KARGA_HOME.
func lockHome(home string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(home, "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another karga-server uses %s", home)
	}
	return func() { _ = f.Close() }, nil
}

// Listen opens the unix socket, replacing a stale one a crashed server left.
func (s *Server) Listen() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(s.cfg.Socket), 0o755); err != nil {
		return nil, err
	}
	if c, err := net.Dial("unix", s.cfg.Socket); err == nil {
		_ = c.Close()
		return nil, fmt.Errorf("a server already listens on %s", s.cfg.Socket)
	}
	if err := os.Remove(s.cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", s.cfg.Socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.cfg.Socket, s.cfg.SocketMode); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Serve answers on l until ctx is done, then waits for running operations (at most grace).
func (s *Server) Serve(ctx context.Context, l net.Listener, grace time.Duration) error {
	hs := &http.Server{Handler: api.NewServer(s.Service, version.Current().Version), ReadHeaderTimeout: 10 * time.Second,
		// Operations record the user of the client process.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return releases.WithActor(ctx, peerUser(c)) }}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(l) }()
	// Operations a stopped karga-server left running are finished first; then the reconciler starts.
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.Service.Resume(context.WithoutCancel(ctx))
		if s.Loop != nil && ctx.Err() == nil {
			s.Loop.Run(ctx)
		}
	}()
	s.cfg.Log.Info("karga-server listening", "socket", s.cfg.Socket, "home", s.cfg.Home, "version", version.Current().Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	// Operations in progress finish (they do not depend on their clients), at most grace.
	err := hs.Shutdown(shutdown)
	select {
	case <-loopDone:
	case <-shutdown.Done():
	}
	_ = os.Remove(s.cfg.Socket)
	return err
}

// Close releases the state.
func (s *Server) Close() error {
	defer s.unlock()
	return s.store.Close()
}
