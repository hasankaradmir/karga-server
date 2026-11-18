// Package envoy writes the file-based configuration of karga's Envoy proxies and reads their admin
// API (ADR-0009, ADR-0010). Switching a cluster's endpoint in cds.yaml moves new connections to
// another container while existing ones finish on the old one.
package envoy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// MountPath is where the proxy directory is mounted inside the Envoy container.
const MountPath = "/etc/karga-proxy"

// DefaultImage is the Envoy image validated in the spikes (1.36.13, distroless: 149MB instead of
// 241MB), pinned by digest so every host runs the same bits. Envoy updates ship with karga
// releases; KARGA_PROXY_IMAGE overrides it.
const DefaultImage = "envoyproxy/envoy:distroless-v1.36.13@sha256:bf82460da76ebdbcec9671ea3d4751d442edcc0632488e9087a1df9dfe0c324e"

// Image returns the proxy image to use.
func Image() string {
	if v := os.Getenv("KARGA_PROXY_IMAGE"); v != "" {
		return v
	}
	return DefaultImage
}

// Command runs Envoy on the mounted bootstrap file with one worker thread (ADR-0010: low resources).
// It starts with a flag, so both the distroless entrypoint (envoy) and the debian one accept it.
func Command() []string {
	return []string{"-c", MountPath + "/envoy.yaml", "--concurrency", "1", "--log-level", "warn"}
}

const (
	// Envoy reads YAML by the file extension.
	bootstrapFile = "envoy.yaml"
	listenersFile = "lds.yaml"
	clustersFile  = "cds.yaml"
	adminSocket   = "admin.sock"
)

// Overload limits of every karga Envoy.
const (
	MaxHeapBytes       = 100 << 20
	MaxConnections     = 4096
	heapMonitor        = "envoy.resource_monitors.fixed_heap"
	connectionsMonitor = "envoy.resource_monitors.global_downstream_max_connections"
)

// The configuration model is the application's (ports): Envoy renders it.
type (
	Cluster      = ports.ProxyCluster
	TCPListener  = ports.ProxyTCPListener
	HTTPListener = ports.ProxyHTTPListener
	Cert         = ports.ProxyCert
	HTTPRoute    = ports.ProxyRoute
	Config       = ports.ProxyConfig
)

// Proxies implements ports.ProxyControl with Envoy (ADR-0009, ADR-0010).
type Proxies struct{}

var _ ports.ProxyControl = Proxies{}

// Container implements ports.ProxyControl.
func (Proxies) Container() ports.ProxyContainer {
	return ports.ProxyContainer{Image: Image(), Command: Command(), MountPath: MountPath,
		MemoryBytes: Memory, MilliCPU: MilliCPU, Pids: Pids,
		Source: fmt.Sprintf("mem=%d,cpu=%dm,pids=%d,log=%sx%d %s", Memory, MilliCPU, Pids, resource.DefaultLogMaxSize, resource.DefaultLogMaxFiles, Image())}
}

// Limits of karga's Envoys: one worker uses about 15MB; the rest is headroom for connections.
const (
	Memory   = 128 << 20
	MilliCPU = 500
	Pids     = 256
)

// Write implements ports.ProxyControl.
func (Proxies) Write(dir, node string, socketMode int, cfg Config) error {
	return Write(dir, node, socketMode, cfg)
}

// Endpoints implements ports.ProxyControl.
func (Proxies) Endpoints(dir string) (map[string]string, error) { return Endpoints(dir) }

// WaitReady implements ports.ProxyControl.
func (Proxies) WaitReady(ctx context.Context, dir string, want map[string]string, timeout time.Duration) error {
	return newAdmin(dir).WaitReady(ctx, want, timeout)
}

// WaitDrained implements ports.ProxyControl.
func (Proxies) WaitDrained(ctx context.Context, dir string, clusters []string, timeout time.Duration) (bool, error) {
	return newAdmin(dir).WaitDrained(ctx, clusters, timeout)
}

// Live implements ports.ProxyControl.
func (Proxies) Live(ctx context.Context, dir string) (bool, error) { return newAdmin(dir).Live(ctx) }

// Version implements ports.ProxyControl.
func (Proxies) Version(ctx context.Context, dir string) (string, error) {
	return newAdmin(dir).Version(ctx)
}

// bootstrap is the static part: an admin API on a unix socket and file-based LDS/CDS. socketMode is
// the admin socket's permission; the proxy directory (0700) is what keeps other users out.
func bootstrap(node string, socketMode int) ([]byte, error) {
	source := func(file string) map[string]any {
		return map[string]any{
			"resource_api_version": "V3",
			"path_config_source": map[string]any{
				"path":              MountPath + "/" + file,
				"watched_directory": map[string]any{"path": MountPath},
			},
		}
	}
	trigger := func(monitor string, at float64) map[string]any {
		return map[string]any{"name": monitor, "threshold": map[string]any{"value": at}}
	}
	return toYAML(map[string]any{
		// Within the container's 128Mi: shed load before the kernel kills Envoy, and cap
		// connections so a flood cannot exhaust it.
		"overload_manager": map[string]any{
			"refresh_interval": "0.25s",
			"resource_monitors": []any{
				map[string]any{"name": heapMonitor, "typed_config": map[string]any{
					"@type":               "type.googleapis.com/envoy.extensions.resource_monitors.fixed_heap.v3.FixedHeapConfig",
					"max_heap_size_bytes": MaxHeapBytes,
				}},
				map[string]any{"name": connectionsMonitor, "typed_config": map[string]any{
					"@type":                             "type.googleapis.com/envoy.extensions.resource_monitors.downstream_connections.v3.DownstreamConnectionsConfig",
					"max_active_downstream_connections": MaxConnections,
				}},
			},
			"actions": []any{
				map[string]any{"name": "envoy.overload_actions.shrink_heap", "triggers": []any{trigger(heapMonitor, 0.90)}},
				map[string]any{"name": "envoy.overload_actions.stop_accepting_requests", "triggers": []any{trigger(heapMonitor, 0.98)}},
			},
		},
		"node":  map[string]any{"id": node, "cluster": "karga"},
		"admin": map[string]any{"address": map[string]any{"pipe": map[string]any{"path": MountPath + "/" + adminSocket, "mode": socketMode}}},
		"dynamic_resources": map[string]any{
			"lds_config": source(listenersFile),
			"cds_config": source(clustersFile),
		},
	})
}

func tcpName(address string, port int) string  { return fmt.Sprintf("tcp_%s_%d", address, port) }
func httpName(address string, port int) string { return fmt.Sprintf("http_%s_%d", address, port) }

// writeAtomic replaces dir/name with a rename, which is the change Envoy's directory watch picks
// up. An unchanged file is left alone so Envoy does not reload for nothing.
func writeAtomic(dir, name string, data []byte) error {
	if old, err := os.ReadFile(filepath.Join(dir, name)); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return fsutil.WriteFile(filepath.Join(dir, name), data, 0o600)
}

// Write creates the proxy directory (0700) and writes bootstrap, clusters and listeners. Clusters
// go first so a new listener never points at a cluster Envoy does not know yet.
func Write(dir, node string, socketMode int, cfg Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	boot, err := bootstrap(node, socketMode)
	if err != nil {
		return err
	}
	cds, err := Clusters(cfg)
	if err != nil {
		return err
	}
	lds, err := Listeners(cfg)
	if err != nil {
		return err
	}
	if err := writeAtomic(dir, bootstrapFile, boot); err != nil {
		return err
	}
	if err := writeAtomic(dir, clustersFile, cds); err != nil {
		return err
	}
	if old, err := os.ReadFile(filepath.Join(dir, listenersFile)); err == nil && bytes.Equal(old, lds) {
		return nil
	}
	// A running Envoy rejects a listener whose route names a cluster that is still warming, and
	// does not retry until the file changes again: wait until it knows every cluster.
	waitClusters(dir, cfg.Clusters, clusterWait)
	return writeAtomic(dir, listenersFile, lds)
}

// clusterWait bounds how long Write waits for a running Envoy to take new clusters.
var clusterWait = 15 * time.Second

func waitClusters(dir string, clusters []Cluster, timeout time.Duration) {
	if _, err := os.Stat(filepath.Join(dir, adminSocket)); err != nil {
		return // not running yet: at startup Envoy loads clusters before listeners
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a := newAdmin(dir)
	for {
		hosts, err := a.hosts(ctx)
		if err == nil && !slices.ContainsFunc(clusters, func(c Cluster) bool { _, ok := hosts[c.Name]; return !ok }) {
			return
		}
		select {
		case <-ctx.Done():
			return // write anyway; WaitReady reports what Envoy then logs
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Endpoints returns each cluster's endpoint as written in dir ("host:port"), empty without config.
func Endpoints(dir string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, clustersFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Resources []struct {
			Name           string `yaml:"name"`
			LoadAssignment struct {
				Endpoints []struct {
					LbEndpoints []struct {
						Endpoint struct {
							Address address `yaml:"address"`
						} `yaml:"endpoint"`
					} `yaml:"lb_endpoints"`
				} `yaml:"endpoints"`
			} `yaml:"load_assignment"`
		} `yaml:"resources"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, clustersFile), err)
	}
	out := map[string]string{}
	for _, r := range cfg.Resources {
		for _, e := range r.LoadAssignment.Endpoints {
			for _, lb := range e.LbEndpoints {
				out[r.Name] = lb.Endpoint.Address.String()
			}
		}
	}
	return out, nil
}

type address struct {
	SocketAddress struct {
		Address   string `json:"address" yaml:"address"`
		PortValue int    `json:"port_value" yaml:"port_value"`
	} `json:"socket_address" yaml:"socket_address"`
}

func (a address) String() string {
	return net.JoinHostPort(a.SocketAddress.Address, strconv.Itoa(a.SocketAddress.PortValue))
}

// Live reports whether Envoy's admin API answers and the server is live.
func (a *Admin) Live(ctx context.Context) (bool, error) {
	body, err := a.get(ctx, "/ready")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(body)) == "LIVE", nil
}

// Version returns the running Envoy's version, e.g. "1.36.13".
func (a *Admin) Version(ctx context.Context) (string, error) {
	body, err := a.get(ctx, "/server_info")
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}
	// "<commit>/<version>/<status>/<build>/<ssl>"
	if parts := strings.Split(info.Version, "/"); len(parts) > 1 {
		return parts[1], nil
	}
	return info.Version, nil
}

// Admin reads a proxy's admin API through the unix socket in its directory.
type Admin struct {
	sock   string
	client *http.Client
}

// adminTransport is shared by every Admin and keeps no connections: a long-running server would
// otherwise gather an idle pool for every proxy it ever talked to. It dials the socket the request's
// context names (every proxy has the same host name, so a kept connection could reach another one).
var adminTransport = &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", ctx.Value(adminSocketKey{}).(string))
	},
	DisableKeepAlives: true,
}

type adminSocketKey struct{}

// newAdmin returns a client for the proxy whose directory is dir.
func newAdmin(dir string) *Admin {
	return &Admin{sock: filepath.Join(dir, adminSocket), client: &http.Client{Timeout: 2 * time.Second, Transport: adminTransport}}
}

func (a *Admin) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.WithValue(ctx, adminSocketKey{}, a.sock), http.MethodGet, "http://envoy"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("envoy admin %s: %s", path, resp.Status)
	}
	return buf.Bytes(), nil
}

type hostStatus struct {
	Hostname     string            `json:"hostname"`
	Address      address           `json:"address"`
	HealthStatus map[string]any    `json:"health_status"`
	Stats        []json.RawMessage `json:"stats"`
}

func (h hostStatus) healthy() bool {
	if h.HealthStatus["eds_health_status"] != "HEALTHY" {
		return false
	}
	for k, v := range h.HealthStatus {
		if strings.HasPrefix(k, "failed_") && v == true {
			return false
		}
	}
	return true
}

// cxActive reads the cx_active gauge; Envoy omits the value when it is 0.
func (h hostStatus) cxActive() int {
	for _, raw := range h.Stats {
		var s struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if json.Unmarshal(raw, &s) == nil && s.Name == "cx_active" {
			n, _ := strconv.Atoi(s.Value)
			return n
		}
	}
	return 0
}

func (a *Admin) hosts(ctx context.Context) (map[string][]hostStatus, error) {
	body, err := a.get(ctx, "/clusters?format=json")
	if err != nil {
		return nil, err
	}
	var d struct {
		ClusterStatuses []struct {
			Name         string       `json:"name"`
			HostStatuses []hostStatus `json:"host_statuses"`
		} `json:"cluster_statuses"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	out := map[string][]hostStatus{}
	for _, c := range d.ClusterStatuses {
		out[c.Name] = c.HostStatuses
	}
	return out, nil
}

// Ready reports whether every cluster in want routes to a healthy host matching its value (the DNS
// name of a DNS cluster, or "ip:port" of a static one) and is used by an active listener, so a
// listener change written with it has been applied too.
func (a *Admin) Ready(ctx context.Context, want map[string]string) (bool, error) {
	why, err := a.notReady(ctx, want)
	return why == "" && err == nil, err
}

// notReady says why want is not served yet ("" when it is).
func (a *Admin) notReady(ctx context.Context, want map[string]string) (string, error) {
	hosts, err := a.hosts(ctx)
	if err != nil {
		return "", err
	}
	used, err := a.listenerClusters(ctx)
	if err != nil {
		return "", err
	}
	for _, cluster := range slices.Sorted(maps.Keys(want)) {
		endpoint := want[cluster]
		hs, known := hosts[cluster]
		switch {
		case !known:
			return fmt.Sprintf("cluster %s is not loaded", cluster), nil
		case !used[cluster]:
			return fmt.Sprintf("no active listener uses cluster %s", cluster), nil
		}
		i := slices.IndexFunc(hs, func(h hostStatus) bool { return h.Hostname == endpoint || h.Address.String() == endpoint })
		switch {
		case i < 0:
			var have []string
			for _, h := range hs {
				have = append(have, h.Address.String())
			}
			return fmt.Sprintf("cluster %s routes to %v, not %s", cluster, have, endpoint), nil
		case !hs[i].healthy():
			return fmt.Sprintf("cluster %s: %s is not healthy (%v)", cluster, endpoint, hs[i].HealthStatus), nil
		}
	}
	return "", nil
}

// listenerClusters returns the clusters the active listeners route to.
func (a *Admin) listenerClusters(ctx context.Context) (map[string]bool, error) {
	body, err := a.get(ctx, "/config_dump?resource=dynamic_listeners")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Configs []struct {
			ActiveState any `json:"active_state"` // warming listeners are not serving yet
		} `json:"configs"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, c := range v {
				if s, ok := c.(string); ok && k == "cluster" {
					out[s] = true
				}
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	for _, c := range doc.Configs {
		walk(c.ActiveState)
	}
	return out, nil
}

// OldConnections counts connections still open to hosts no longer in the clusters: each cluster's
// upstream_cx_active (all connections ever opened under that name) minus those of its current hosts.
func (a *Admin) OldConnections(ctx context.Context, clusters []string) (int, error) {
	hosts, err := a.hosts(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, name := range clusters {
		body, err := a.get(ctx, "/stats?filter=^cluster\\."+regexp.QuoteMeta(name)+"\\.upstream_cx_active$")
		if err != nil {
			return 0, err
		}
		_, v, _ := strings.Cut(strings.TrimSpace(string(body)), ": ")
		all, _ := strconv.Atoi(v)
		current := 0
		for _, h := range hosts[name] {
			current += h.cxActive()
		}
		total += max(0, all-current)
	}
	return total, nil
}

// WaitReady polls Ready until it is true or timeout passes.
func (a *Admin) WaitReady(ctx context.Context, want map[string]string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	why := ""
	for time.Now().Before(deadline) {
		w, err := a.notReady(ctx, want)
		if w == "" && err == nil {
			return nil
		}
		lastErr, why = err, w
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("proxy does not route to %s after %s: %w", describe(want), timeout, lastErr)
	}
	return fmt.Errorf("proxy does not route to a healthy %s after %s: %s", describe(want), timeout, why)
}

// WaitDrained polls OldConnections until it is 0 or timeout passes; it reports whether it drained.
func (a *Admin) WaitDrained(ctx context.Context, clusters []string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		n, err := a.OldConnections(ctx, clusters)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func describe(want map[string]string) string {
	var parts []string
	for _, c := range slices.Sorted(maps.Keys(want)) {
		parts = append(parts, want[c])
	}
	return strings.Join(slices.Compact(parts), ", ")
}

// toYAML writes v as YAML with sorted keys: equal configurations give equal files.
func toYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
