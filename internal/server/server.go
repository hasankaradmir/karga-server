// Package server runs karga-server: it owns the state and the container runtime and
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

	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"

	"github.com/hasankaradmir/karga-server/internal/infrastructure/logging"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
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
	// Redactor masks the secrets the service registers in everything Log writes; Log must use it.
	// Nil: secrets are registered nowhere (tests).
	Redactor *logging.Redactor
	// AllowedCaps are the Linux capabilities, besides the default set, packages may add.
	AllowedCaps []string
	// ReconcileInterval is the time between full reconciliation passes (0: events only); a
	// negative value turns reconciliation off (tests).
	ReconcileInterval time.Duration
	// EdgeSubnet is the address range of the network the gateway reaches release proxies on
	// (empty: proxyops.DefaultEdgeSubnet).
	EdgeSubnet string
}

// DefaultReconcileInterval is how often every release is looked at besides runtime events.
const DefaultReconcileInterval = 30 * time.Second

// classify gives karga-server's errors their kind for the client; anything else is internal.
func classify(err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, ports.ErrImageNotFound):
		return api.ErrNotFound
	case errors.Is(err, ports.ErrInvalid):
		return api.ErrInvalid
	case errors.Is(err, releases.ErrLocked), errors.Is(err, releases.ErrInterrupted), errors.Is(err, releases.ErrExists),
		errors.Is(err, ports.ErrPortInUse), errors.Is(err, ports.ErrConflict):
		return api.ErrConflict
	case errors.Is(err, releases.ErrNotReady):
		return api.ErrNotReady
	}
	return nil
}

// Server is a running karga-server.
type Server struct {
	cfg    Config
	svc    *releases.Service
	loop   *reconciler.Loop
	store  *filestore.Store
	unlock func()
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
	// Uploaded packages and volume backups wait here, private to karga-server.
	if err := os.MkdirAll(filepath.Join(cfg.Home, "tmp"), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockHome(cfg.Home)
	if err != nil {
		return nil, err
	}
	store, err := filestore.Open(cfg.Home)
	if err != nil {
		unlock()
		return nil, err
	}
	var redact func(string)
	var mask func(string) string
	if cfg.Redactor != nil {
		redact, mask = cfg.Redactor.Add, cfg.Redactor.Redact
	}
	// The composition root: Envoy runs the proxies, the host's network runs probes and port tests.
	host := &proxyops.Host{Proxies: envoy.Proxies{}, Net: hostnet.Net{}, EdgeSubnet: cfg.EdgeSubnet}
	// The queue comes first: the service tells it what changed, the loop takes from it.
	q := reconciler.NewQueue(0)
	notify := q.Notify
	if cfg.ReconcileInterval < 0 {
		notify = nil
	}
	svc, err := releases.New(releases.Deps{Store: store, Runtime: rt,
		Host: host, Home: cfg.Home, Redact: redact, Mask: mask, Log: cfg.Log, AllowedCaps: cfg.AllowedCaps, Notify: notify})
	if err != nil {
		_ = store.Close()
		unlock()
		return nil, err
	}
	info, err := rt.Info(ctx)
	if err != nil {
		_ = store.Close()
		unlock()
		return nil, err
	}
	host.UseRuntime(info)
	s := &Server{cfg: cfg, svc: svc, store: store, unlock: unlock}
	if cfg.ReconcileInterval >= 0 {
		s.loop = reconciler.New(q, svc.Reads, svc.Lifecycle, rt, reconciler.Options{Interval: cfg.ReconcileInterval, Log: cfg.Log})
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

// mask masks the secret values the service registered (none without a Redactor).
func (s *Server) mask(v string) string {
	if s.cfg.Redactor == nil {
		return v
	}
	return s.cfg.Redactor.Redact(v)
}

// Serve answers on l until ctx is done, then waits for running operations (at most grace).
func (s *Server) Serve(ctx context.Context, l net.Listener, grace time.Duration) error {
	started := time.Now()
	// Nothing runs yet: what an earlier process left in tmp (uploads, packages, backup copies) goes.
	s.removeLeftovers(filepath.Join(s.cfg.Home, "tmp"), started)
	streams, endStreams := context.WithCancel(context.Background())
	defer endStreams()
	hs := &http.Server{Handler: api.NewServer(NewBackend(s.svc, streams), version.Current().Version, api.Options{TempDir: filepath.Join(s.cfg.Home, "tmp"), Classify: classify, Redact: s.mask}), ReadHeaderTimeout: 10 * time.Second,
		// Operations record the user of the client process.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return releases.WithActor(ctx, peerUser(c)) }}
	// Secret values are masked in the log from the start, not only once a release is applied again.
	s.svc.RegisterSecrets(ctx)
	// Operations a stopped karga-server left running are resumed in the background: Resume holds
	// their releases first, so a command on one waits for its resume and never meets it half done,
	// while every other release is served at once. The reconciler starts when they are done.
	resumed := s.svc.Lifecycle.Resume(ctx)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(l) }()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		select {
		case <-resumed:
		case <-ctx.Done():
			return
		}
		// The resumed restores took the backups they kept; the others' are left over.
		s.removeLeftovers(filepath.Join(s.cfg.Home, "restores"), started)
		if s.loop != nil {
			s.loop.Run(ctx)
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
	endStreams() // log streams and exec sessions end; operations are waited for
	// Operations in progress finish (they do not depend on their clients), at most grace.
	err := hs.Shutdown(shutdown)
	for _, done := range []<-chan struct{}{loopDone, resumed} {
		select {
		case <-done:
		case <-shutdown.Done():
		}
	}
	_ = os.Remove(s.cfg.Socket)
	return err
}

// removeLeftovers removes the files in dir older than before (an earlier process's), logging what
// it cannot.
func (s *Server) removeLeftovers(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // none yet
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			s.cfg.Log.Warn("remove leftover", "path", filepath.Join(dir, e.Name()), "error", err)
		} else {
			s.cfg.Log.Info("removed leftover", "path", filepath.Join(dir, e.Name()))
		}
	}
}

// Close releases the state.
func (s *Server) Close() error {
	defer s.unlock()
	return s.store.Close()
}
