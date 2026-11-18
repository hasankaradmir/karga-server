package ports

import (
	"context"
	"time"
)

// HostNet is what karga does on the host's network itself, outside the container runtime.
type HostNet interface {
	// Probe runs one tcp or http check: a connection to Addr, or a GET of Path on it that answers
	// 2xx or 3xx, within Timeout.
	Probe(ctx context.Context, p Probe) error
	// PortTaken test-binds a host port: taken when another process holds it. err says the port could
	// not be tested (binding it needs privileges).
	PortTaken(protocol, ip string, port int) (taken bool, err error)
}

// Probe is one health check of a workload.
type Probe struct {
	Type    string // tcp or http
	Addr    string // host:port
	Path    string // http only
	Timeout time.Duration
}
