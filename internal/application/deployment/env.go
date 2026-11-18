package deployment

import (
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
)

// Env is what deployments need from where they run: the host's proxies, network and runtime
// support, and the key of the digests of secret material. karga-server makes one per state home.
type Env struct {
	*proxyops.Host
	// DigestKey keys the digests of secret material: without it a weak secret could be guessed
	// offline from the state. It comes from $KARGA_HOME/state.key.
	DigestKey []byte
	// PollInterval is how often a wait checks workload status (default 500ms); WaitEvery how often a
	// long wait reports that it still waits (default 2s).
	PollInterval, WaitEvery time.Duration
}

func (e *Env) pollInterval() time.Duration { return or(e.PollInterval, 500*time.Millisecond) }
func (e *Env) waitEvery() time.Duration    { return or(e.WaitEvery, 2*time.Second) }

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
