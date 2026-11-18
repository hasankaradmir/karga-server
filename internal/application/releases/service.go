// Package releases implements the release lifecycle: install, upgrade, rollback, uninstall and reads.
package releases

import (
	"errors"
	"log/slog"
	"os"
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

// Service is the release service. Its faces share one core: the state, the runtime, the locks and
// the deploy steps; nothing outside the package reaches it.
type Service struct {
	// Lifecycle installs, upgrades, rolls back, uninstalls, resumes and repairs releases.
	Lifecycle *Lifecycle
	// Reads answers questions about releases; it changes nothing.
	Reads *Reads
	// Containers reach into a release's containers: logs and exec.
	Containers *Containers
	// Volumes backs up and restores a release's volumes.
	Volumes *Volumes
	// Gateway reads and changes the host gateway.
	Gateway *Gateway

	c *core
}

// Lifecycle changes releases.
type Lifecycle struct{ c *core }

// Reads reads releases; it changes nothing.
type Reads struct{ c *core }

// Containers reach into a release's containers.
type Containers struct{ c *core }

// Volumes copies volume data in and out.
type Volumes struct{ c *core }

// Gateway holds the host gateway's settings and health.
type Gateway struct{ c *core }

// core is what the faces share. It is set up by New and does not change after.
type core struct {
	store ports.StateStore
	// env plans and applies deployments on this host.
	env      *deployment.Env
	rt       ports.Runtime
	home     string
	redact   func(string) // registers secret values with the log redactor
	log      *slog.Logger
	now      func() time.Time
	pid      int
	hostname string
	// notify is told the name of a release an install, upgrade or rollback changed.
	notify func(name string)

	repairs repairs
	locks   opLocks
}

// Deps are what the release service is made of.
type Deps struct {
	Store ports.StateStore
	// Runtime is the container runtime. Whether it can run karga's proxies is Host's to know
	// (Host.UseRuntime, once, where the service is made).
	Runtime ports.Runtime
	// Host is where deployments run: its proxies and network.
	Host *proxyops.Host
	// Home is $KARGA_HOME; its state.key is created on first use.
	Home string
	// Redact registers secret values with the log redactor (nil: none).
	Redact func(string)
	Log    *slog.Logger
	// AllowedCaps are the Linux capabilities, besides resource.DefaultCaps, the host administrator
	// lets packages add (karga-server --allow-capabilities).
	AllowedCaps []string
	// Notify, if set, is told the name of a release an install, upgrade or rollback changed (the
	// reconciler's queue).
	Notify func(name string)
}

// New returns the release service of d, with process defaults filled in. It reads (or creates) the
// state key, so every digest is keyed from the start.
func New(d Deps) (*Service, error) {
	key, err := loadKey(d.Home)
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	if d.Redact == nil {
		d.Redact = func(string) {}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	env := deployment.NewEnv(d.Host, deployment.EnvConfig{Home: d.Home, DigestKey: key, Redact: d.Redact, AllowedCaps: d.AllowedCaps})
	if d.Notify == nil {
		d.Notify = func(string) {}
	}
	c := &core{store: d.Store, env: env, rt: d.Runtime, home: d.Home, redact: d.Redact, log: d.Log, now: time.Now,
		pid: os.Getpid(), hostname: hostname, notify: d.Notify}
	return &Service{Lifecycle: &Lifecycle{c}, Reads: &Reads{c}, Containers: &Containers{c}, Volumes: &Volumes{c}, Gateway: &Gateway{c}, c: c}, nil
}
