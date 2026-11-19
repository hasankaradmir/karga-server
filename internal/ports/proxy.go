package ports

import (
	"context"
	"time"
)

// The configuration of karga's proxies (the release proxies and the gateway) and their control.
// The application builds ProxyConfig; a ProxyControl writes it in the proxy's own format.

// ProxyCluster is an upstream with one endpoint. With DNS the host is a name resolved continuously
// (container restarts and IP changes do not matter); otherwise it is an IP address.
type ProxyCluster struct {
	Name string
	Host string
	Port int
	DNS  bool
	// HealthPath makes the health check an HTTP GET of that path (2xx/3xx); empty: a TCP connect.
	HealthPath string
	// ProxyProtocol ("v1", "v2") sends the client's address first on every upstream connection.
	ProxyProtocol string
}

// ProxyTCPListener proxies every connection on Address:Port to Cluster.
type ProxyTCPListener struct {
	Address string
	Port    int
	Cluster string
	// IdleTimeout closes a connection idle that long (0: the proxy's default).
	IdleTimeout time.Duration
}

// ProxyHTTPListener serves HTTP on Address:Port and routes requests by Host header and path prefix.
// With Certs it serves HTTPS: each host gets its certificate by SNI. Redirects lists hosts whose
// plain requests are sent to HTTPS on RedirectPort.
type ProxyHTTPListener struct {
	Address      string
	Port         int
	Routes       []ProxyRoute
	Certs        map[string]ProxyCert // host -> certificate files, inside the container
	Redirects    []string
	RedirectPort int
	Limits       ProxyHTTPLimits
}

// ProxyHTTPLimits bound the requests of an HTTP listener; zero values mean no limit (IdleTimeout:
// the proxy's default).
type ProxyHTTPLimits struct {
	// Timeout bounds a whole request; IdleTimeout a request that sends nothing either way.
	Timeout, IdleTimeout time.Duration
	// MaxBodyBytes refuses larger request bodies.
	MaxBodyBytes uint32
	// RateRequests requests are allowed every RatePer.
	RateRequests uint32
	RatePer      time.Duration
}

// ProxyCert is a certificate chain and private key, as file paths the proxy reads.
type ProxyCert struct {
	Chain, Key string
}

// ProxyRoute sends requests for Host whose path starts with Path to Cluster.
type ProxyRoute struct {
	Host    string
	Path    string
	Cluster string
}

// ProxyConfig is the dynamic part of a proxy: listeners and clusters. AccessLog writes a line per
// request (HTTP) or connection (TCP) to stdout.
type ProxyConfig struct {
	TCP       []ProxyTCPListener
	HTTP      []ProxyHTTPListener
	Clusters  []ProxyCluster
	AccessLog bool
}

// ProxyContainer is how a proxy container runs: its image, command, and where its directory is
// mounted inside it.
type ProxyContainer struct {
	Image     string
	MountPath string
	// Limits of the container.
	MemoryBytes, MilliCPU, Pids int64
	// Source names what the container is made from (image and limits): a proxy whose recorded
	// source differs is replaced.
	Source string
}

// ProxyControl writes a proxy's configuration into its directory and reads its state, for the
// release proxies and the gateway alike.
type ProxyControl interface {
	Container() ProxyContainer
	Write(dir string, cfg ProxyConfig) error
	// WriteInstance prepares a second Envoy of dir, in dir/instance: it serves dir's configuration
	// with its own admin socket, whose directory (dir/instance) the other methods then take. Command
	// runs it ("" runs dir's own).
	WriteInstance(dir, instance, node string, socketMode int) error
	Command(instance string) []string
	// Drain makes the proxy stop taking new work and waits until its connections are gone, or
	// timeout; it reports whether they all finished. Graceful keeps the listeners open meanwhile
	// and asks clients to go (for a proxy found by DNS); otherwise they close at once (for one
	// sharing its ports with its successor, which then takes the new connections).
	Drain(ctx context.Context, dir string, graceful bool, timeout time.Duration) (bool, error)
	// Endpoints returns each cluster's endpoint as written in dir ("host:port").
	Endpoints(dir string) (map[string]string, error)
	// WaitReady waits until each cluster routes to a healthy endpoint (cluster -> DNS name or ip:port).
	WaitReady(ctx context.Context, dir string, want map[string]string, timeout time.Duration) error
	// WaitDrained waits until no connection is left on endpoints the clusters no longer have.
	WaitDrained(ctx context.Context, dir string, clusters []string, timeout time.Duration) (bool, error)
	// Live reports whether the proxy answers; Version is its version.
	Live(ctx context.Context, dir string) (bool, error)
	Version(ctx context.Context, dir string) (string, error)
}
