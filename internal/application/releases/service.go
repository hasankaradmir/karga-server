// Package releases implements the release lifecycle: install, upgrade, rollback, uninstall and reads.
package releases

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Errors callers can check.
var (
	ErrLocked      = errors.New("another operation is running on this release")
	ErrInterrupted = errors.New("a previous operation on this release was interrupted")
	ErrExists      = errors.New("release already exists")
)

// DefaultHistoryMax is how many revisions are kept per release unless a request says otherwise (D13).
const DefaultHistoryMax = 10

// historyMax is the number of revisions a request keeps (0: all).
func historyMax(n *int) int {
	if n == nil {
		return DefaultHistoryMax
	}
	return *n
}

// invalid marks an error as the caller's (a package that does not render, values that do not
// validate): it is ports.ErrInvalid. The message is unchanged.
type invalid struct{ error }

func (e invalid) Unwrap() error { return e.error }

func (invalid) Is(target error) bool { return target == ports.ErrInvalid }

// Service is the release service: one Core shared by its four faces.
type Service struct {
	*Core
	// Lifecycle installs, upgrades, rolls back, uninstalls, resumes and repairs releases.
	Lifecycle *Lifecycle
	// Reads answers questions about releases and reaches into their containers (logs, exec).
	Reads *Reads
	// Volumes backs up and restores a release's volumes.
	Volumes *Volumes
	// Gateway reads and changes the host gateway.
	Gateway *Gateway
}

// Lifecycle changes releases.
type Lifecycle struct{ *Core }

// Reads reads releases; it changes nothing.
type Reads struct{ *Core }

// Volumes copies volume data in and out.
type Volumes struct{ *Core }

// Gateway holds the host gateway's settings and health.
type Gateway struct{ *Core }

// Core is what the faces share: the state, the runtime, the locks and the deploy steps. Runtime is
// opened lazily so read-only commands never need Docker.
type Core struct {
	// AllowedCaps are the Linux capabilities, besides resource.DefaultCaps, the host administrator
	// lets packages add (karga-server --allow-capabilities).
	AllowedCaps []string
	Store       ports.StateStore
	// Deploy plans and applies deployments on this host.
	Deploy  *deployment.Env
	Runtime func(context.Context) (ports.Runtime, error)
	Home    string
	// Redact registers secret values with the log redactor.
	Redact   func(string)
	Log      *slog.Logger
	Now      func() time.Time
	PID      int
	Hostname string

	// AfterChange, if set, is told the name of a release an install, upgrade or rollback changed.
	AfterChange func(name string)

	keyMu   sync.Mutex
	key     []byte // nil until loaded: a failed load is tried again next time
	repairs repairs
	// busy serializes the operations of a release inside this process: a user's command waits for
	// a reconciliation in progress instead of failing; a reconciliation skips a busy release.
	busyMu sync.Mutex
	busy   map[string]*releaseLock // releases someone holds or waits for
}

// New returns a Service with process defaults filled in.
func New(store ports.StateStore, rt func(context.Context) (ports.Runtime, error), host *proxyops.Host, home string, redact func(string), log *slog.Logger) *Service {
	hostname, _ := os.Hostname()
	if redact == nil {
		redact = func(string) {}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Core{Store: store, Deploy: &deployment.Env{Host: host}, Runtime: rt, Home: home, Redact: redact, Log: log, Now: time.Now, PID: os.Getpid(), Hostname: hostname}
	return &Service{Core: c, Lifecycle: &Lifecycle{c}, Reads: &Reads{c}, Volumes: &Volumes{c}, Gateway: &Gateway{c}}
}
