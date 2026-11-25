package deployment

import (
	"log/slog"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Env is what deployments need from where they run: the host's proxies, network and runtime
// support, the state home, and the key of the digests of secret material. karga-server makes one
// per state home; nothing in it changes after NewEnv.
type Env struct {
	host        *proxyops.Host
	key         []byte
	home        string
	redact      func(string)
	allowedCaps []string
	poll, every time.Duration
	log         *slog.Logger
}

// EnvConfig configures NewEnv.
type EnvConfig struct {
	// Home is the state home ($KARGA_HOME): revisions' Config and Secret files and proxy
	// configurations are kept under it.
	Home string
	// DigestKey keys the digests of secret material: without it a weak secret could be guessed
	// offline from the state. It comes from $KARGA_HOME/state.key.
	DigestKey []byte
	// Redact receives every secret value before anything is logged (nil: none).
	Redact func(string)
	// AllowedCaps are the capabilities beyond resource.DefaultCaps workloads may add.
	AllowedCaps []string
	// PollInterval is how often a wait checks workload status (default 500ms); WaitEvery how often a
	// long wait reports that it still waits (default 2s).
	PollInterval, WaitEvery time.Duration
	// Log receives what the gateway leaves out (nil: nothing).
	Log *slog.Logger
}

// NewEnv returns the deployment environment of host.
func NewEnv(host *proxyops.Host, c EnvConfig) *Env {
	if c.Redact == nil {
		c.Redact = func(string) {}
	}
	return &Env{host: host, key: c.DigestKey, home: c.Home, redact: c.Redact, allowedCaps: c.AllowedCaps,
		poll: or(c.PollInterval, 500*time.Millisecond), every: or(c.WaitEvery, 2*time.Second), log: c.Log}
}

// gateway is the host gateway, logging to the environment's log.
func (e *Env) gateway(rt ports.Runtime) *gateway.Gateway {
	return gateway.New(rt, e.host, e.home).WithLog(e.log)
}

// Host is the host the deployments run on.
func (e *Env) Host() *proxyops.Host { return e.host }

// Home is the state home.
func (e *Env) Home() string { return e.home }

func (e *Env) pollInterval() time.Duration { return e.poll }
func (e *Env) waitEvery() time.Duration    { return e.every }

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
