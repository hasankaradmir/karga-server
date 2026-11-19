// Package gateway runs the host gateway (ADR-0011): one Envoy in the host network namespace that
// serves the listeners the host's administrator defines and forwards to the release proxies'
// loopback upstream ports. Packages only attach Services to listeners; they cannot open one.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// The gateway is the host's single Envoy: it belongs to no release; karga creates it when a Service
// is exposed and removes it when none is.
const (
	ContainerName = "karga-gateway"
	// LabelPorts on a release proxy container lists the loopback upstream ports it publishes its
	// exposed service ports on, as JSON ([]Port).
	LabelPorts     = "io.karga.gateway"
	LabelComponent = "io.karga.component"
)

// Port is a service port a release proxy publishes on a loopback upstream port for the gateway.
type Port struct {
	Service  string `json:"service"`
	Port     int    `json:"port"`
	Upstream int    `json:"upstream"`
}

// Route attaches a service port to a gateway listener: by Host and Path (a prefix) on an http or
// https listener, or alone on a tcp one.
type Route struct {
	Service  string `yaml:"service"`
	Port     int    `yaml:"port"`
	Listener string `yaml:"listener"`
	Host     string `yaml:"host,omitempty"`
	Path     string `yaml:"path,omitempty"`
	// TLSSecret and CertDir (its materialized directory, with tls.crt and tls.key) serve HTTPS.
	TLSSecret string `yaml:"tlsSecret,omitempty"`
	CertDir   string `yaml:"certDir,omitempty"`
	// HealthPath is checked by HTTP instead of a TCP connect (the workload's http health check).
	HealthPath string `yaml:"healthPath,omitempty"`
}

// Target is one release proxy the gateway forwards to, with what its release exposes.
type Target struct {
	Release, Container string
	Ports              []Port
	Routes             []Route
}

func (t Target) upstream(service string, port int) (int, bool) {
	i := slices.IndexFunc(t.Ports, func(p Port) bool { return p.Service == service && p.Port == port })
	if i < 0 {
		return 0, false
	}
	return t.Ports[i].Upstream, true
}

// Gateway is the host gateway as karga-server runs it: a container on rt, configured through the
// host's proxies, with its files under home.
type Gateway struct {
	rt   Runtime
	host *proxyops.Host
	home string
}

// New returns the host gateway on rt; home is $KARGA_HOME.
func New(rt Runtime, host *proxyops.Host, home string) *Gateway {
	return &Gateway{rt: rt, host: host, home: home}
}

// Runtime is what the host gateway needs of a container runtime.
type Runtime interface {
	proxyops.Remover
	EnsureImage(ctx context.Context, ref, policy string) error
	CreateWorkload(ctx context.Context, req ports.WorkloadRequest) error
	StartWorkload(ctx context.Context, name string) error
	ListLabeled(ctx context.Context, key string) ([]ports.Object, error)
}

// findTargets lists every release proxy that publishes upstream ports, with its release's routes.
func findTargets(ctx context.Context, rt Runtime, home string) ([]Target, error) {
	objs, err := rt.ListLabeled(ctx, LabelPorts)
	if err != nil {
		return nil, err
	}
	var out []Target
	for _, o := range objs {
		rel := o.Labels[ports.LabelRelease]
		routes, err := readExpose(home, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, Target{Release: rel, Container: o.Name, Ports: ParseLabel(o.Labels), Routes: routes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release < out[j].Release })
	return out, nil
}

// Sync rewrites the gateway from the settings and every release's proxy, and creates or removes
// the gateway container as needed. It returns the targets it routes to.
func (g *Gateway) Sync(ctx context.Context) ([]Target, error) {
	rt, h, home := g.rt, g.host, g.home
	unlock, err := Lock(ctx, home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return syncLocked(ctx, rt, h, home)
}

// Expose records what release rel exposes and syncs the gateway, checking again under the gateway
// lock that no other release took the same listeners, hosts or paths since the pre-flight.
func (g *Gateway) Expose(ctx context.Context, rel string, rs []resource.Resource, routes []Route) ([]Target, error) {
	rt, h, home := g.rt, g.host, g.home
	unlock, err := Lock(ctx, home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Preflight reported the warnings; this check runs again under the lock only for the errors.
	if _, err := g.Check(ctx, rel, rs); err != nil {
		return nil, err
	}
	if err := writeExpose(home, rel, routes); err != nil {
		return nil, err
	}
	return syncLocked(ctx, rt, h, home)
}

func syncLocked(ctx context.Context, rt Runtime, h *proxyops.Host, home string) ([]Target, error) {
	c := h.Proxies.Container()
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return nil, err
	}
	dir := Dir(home)
	exposed := slices.ContainsFunc(targets, func(t Target) bool { return len(t.Routes) > 0 })
	// The host has one gateway. Another KARGA_HOME's is never replaced nor removed: two servers
	// would replace each other's for ever (one karga-server per host).
	if st, err := rt.GetWorkload(ctx, ContainerName); err == nil && !strings.HasPrefix(st.Labels[labelSource], dir+" ") {
		if !exposed {
			return nil, os.RemoveAll(dir)
		}
		other, _, _ := strings.Cut(st.Labels[labelSource], " ")
		return nil, fmt.Errorf("%w: the host gateway %s is the one of the karga-server with its state in %s; one karga-server runs per host",
			ports.ErrConflict, ContainerName, filepath.Dir(other))
	}
	if !exposed {
		if err := proxyops.RemoveWorkload(ctx, rt, ContainerName); err != nil && !errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}
		return nil, os.RemoveAll(dir)
	}
	settings, err := ReadSettings(home)
	if err != nil {
		return nil, err
	}
	// Certificates go first: a listener referencing a missing file would be rejected.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := syncCerts(dir, targets, false); err != nil {
		return nil, err
	}
	cfg := buildConfig(settings, targets, c.MountPath)
	cfg.AccessLog = settings.AccessLog
	// The socket is created by root in the 0700 directory, which keeps other users out.
	if err := h.Proxies.Write(dir, "gateway", 0o666, cfg); err != nil {
		return nil, err
	}
	if err := syncCerts(dir, targets, true); err != nil {
		return nil, err
	}
	st, err := rt.GetWorkload(ctx, ContainerName)
	if err == nil && st.Labels[labelSource] != source(c, dir) {
		if err := proxyops.RemoveWorkload(ctx, rt, ContainerName); err != nil {
			return nil, err
		}
		err = fmt.Errorf("gateway %w", ports.ErrNotFound)
	}
	switch {
	case errors.Is(err, ports.ErrNotFound):
		if err := rt.EnsureImage(ctx, c.Image, "if-not-present"); err != nil {
			return nil, err
		}
		if err := rt.CreateWorkload(ctx, request(c, dir)); err != nil {
			return nil, err
		}
		err = rt.StartWorkload(ctx, ContainerName)
	case err == nil && st.State != ports.StateRunning:
		err = rt.StartWorkload(ctx, ContainerName)
	}
	return targets, err
}

// Wait waits until the gateway routes to every exposed port of release rel.
func (g *Gateway) Wait(ctx context.Context, rel string, targets []Target) error {
	h, home := g.host, g.home
	settings, err := ReadSettings(home)
	if err != nil {
		return err
	}
	want := Want(settings, targets, func(t Target) bool { return t.Release == rel && len(t.Routes) > 0 })
	if len(want) == 0 {
		return nil
	}
	return h.Proxies.WaitReady(ctx, Dir(home), want, proxyops.ProxyReadyTimeout)
}

// Listener protocols.
const (
	ProtocolTCP   = "tcp"
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
)

// State is the health of the host gateway.
type State struct {
	// Container is the gateway container's state, or "missing" when no release needs it.
	Container string
	Ready     bool
	Version   string
	Image     string
	Listen    []string
	AccessLog bool
	// Error says why the admin API could not be read, if it could not.
	Error string
}

// Health reads the gateway container and Envoy's admin API.
func (g *Gateway) Health(ctx context.Context) (State, error) {
	rt, h, home := g.rt, g.host, g.home
	settings, err := ReadSettings(home)
	if err != nil {
		return State{}, err
	}
	st := State{AccessLog: settings.AccessLog, Listen: settings.Listen}
	ws, err := rt.GetWorkload(ctx, ContainerName)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		st.Container = "missing"
		return st, nil
	case err != nil:
		return st, err
	}
	st.Container = ws.State
	if src := ws.Labels[labelSource]; strings.Contains(src, " ") {
		st.Image = src[strings.LastIndex(src, " ")+1:]
	}
	if st.Ready, err = h.Proxies.Live(ctx, Dir(home)); err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Version, _ = h.Proxies.Version(ctx, Dir(home))
	return st, nil
}

// Exposed is a service port the gateway serves on a listener.
type Exposed struct {
	Listener string // name:port
	Route    string // "tcp" or host+path
	Release  string
	Service  string // name:port
	Upstream string
}

// Routes lists what the gateway serves, as computed from the release proxies.
func (g *Gateway) Routes(ctx context.Context) ([]Exposed, error) {
	rt, home := g.rt, g.home
	settings, err := ReadSettings(home)
	if err != nil {
		return nil, err
	}
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return nil, err
	}
	var out []Exposed
	for _, t := range targets {
		for _, r := range t.Routes {
			up, ok := t.upstream(r.Service, r.Port)
			l, defined := listenerOf(settings, r.Listener)
			if !ok || !defined {
				continue
			}
			route := "tcp"
			if r.Host != "" {
				route = r.Host + r.Path
			}
			out = append(out, Exposed{Listener: fmt.Sprintf("%s:%d", l.Name, l.Port), Route: route, Release: t.Release,
				Service: fmt.Sprintf("%s:%d", r.Service, r.Port), Upstream: net.JoinHostPort("127.0.0.1", strconv.Itoa(up))})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Listener != out[j].Listener {
			return out[i].Listener < out[j].Listener
		}
		return out[i].Route < out[j].Route
	})
	return out, nil
}

// Users returns, per listener, the release/service names that expose something on it.
func (g *Gateway) Users(ctx context.Context) (map[string][]string, error) {
	rt, home := g.rt, g.home
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, t := range targets {
		for _, r := range t.Routes {
			if u := t.Release + "/" + r.Service; !slices.Contains(out[r.Listener], u) {
				out[r.Listener] = append(out[r.Listener], u)
			}
		}
	}
	return out, nil
}
