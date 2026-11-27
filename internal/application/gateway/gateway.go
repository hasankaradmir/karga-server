// Package gateway runs the host gateway (ADR-0011): one Envoy in the host network namespace that
// serves the listeners the host's administrator defines and forwards to the release proxies'
// loopback upstream ports. Packages only attach Services to listeners; they cannot open one.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	ContainerName  = "karga-gateway"
	LabelComponent = "io.karga.component"
)

// Port is a service port of a release proxy the gateway forwards to, at the proxy's edge address
// (D42).
type Port struct {
	Service string
	Port    int
}

// Route attaches a service port to a gateway listener: by Host and Path (a prefix) on an http or
// https listener, or alone on a tcp one.
type Route struct {
	Service  string `yaml:"service"`
	Port     int    `yaml:"port"`
	Listener string `yaml:"listener"`
	Host     string `yaml:"host,omitempty"`
	// Path and PathType (prefix, by segment, or exact) select the requests; To (with a prefix)
	// replaces the prefix in what the service receives.
	Path     string `yaml:"path,omitempty"`
	PathType string `yaml:"pathType,omitempty"`
	To       string `yaml:"to,omitempty"`
	// TLSSecret and CertDir (its materialized directory, with tls.crt and tls.key) serve HTTPS.
	TLSSecret string `yaml:"tlsSecret,omitempty"`
	CertDir   string `yaml:"certDir,omitempty"`
}

// key is what the route takes on its listener (resource.Exposure.RouteKey).
func (r Route) key() string {
	return resource.Exposure{Host: r.Host, Path: r.Path, PathType: r.PathType}.RouteKey()
}

// Target is one release proxy the gateway forwards to, with what its release exposes.
type Target struct {
	Release, Container string
	// IP is the proxy's address on the edge network.
	IP     string
	Ports  []Port
	Routes []Route
}

// upstream is where the gateway reaches a service port of the target: its proxy's edge address
// and the service port itself.
func (t Target) upstream(service string, port int) (string, bool) {
	if !slices.ContainsFunc(t.Ports, func(p Port) bool { return p.Service == service && p.Port == port }) {
		return "", false
	}
	return net.JoinHostPort(t.IP, strconv.Itoa(port)), true
}

// Gateway is the host gateway as karga-server runs it: a container on rt, configured through the
// host's proxies, with its files under home.
type Gateway struct {
	rt   Runtime
	host *proxyops.Host
	home string
	log  *slog.Logger
}

// New returns the host gateway on rt; home is $KARGA_HOME.
func New(rt Runtime, host *proxyops.Host, home string) *Gateway {
	return &Gateway{rt: rt, host: host, home: home}
}

// WithLog makes g log the releases it leaves out (their files cannot be read).
func (g *Gateway) WithLog(log *slog.Logger) *Gateway {
	g.log = log
	return g
}

// leftOut reports a release the gateway leaves out: one release's broken files do not stop the
// others' routes.
func (g *Gateway) leftOut(rel string, err error) {
	if g.log != nil {
		g.log.Warn("gateway: release left out", "release", rel, "error", err)
	}
}

// Runtime is what the host gateway needs of a container runtime.
type Runtime interface {
	proxyops.Remover
	EnsureImage(ctx context.Context, ref, policy string, progress func(current, total int64)) error
	CreateWorkload(ctx context.Context, req ports.WorkloadRequest) error
	StartWorkload(ctx context.Context, name string) error
	ListLabeled(ctx context.Context, key string) ([]ports.Object, error)
	Logs(ctx context.Context, name string, tail int, follow bool, w io.Writer) error
}

// findTargets lists every release proxy that publishes upstream ports, with its release's routes.
func (g *Gateway) findTargets(ctx context.Context) ([]Target, error) {
	rt, home := g.rt, g.home
	objs, err := rt.ListLabeled(ctx, LabelEdgeIP)
	if err != nil {
		return nil, err
	}
	var out []Target
	revisions := map[string]int{}
	revision := func(o ports.Object) int { n, _ := strconv.Atoi(o.Labels[ports.LabelRevision]); return n }
	for _, o := range objs {
		rel := o.Labels[ports.LabelRelease]
		proxy, routes, err := readExpose(home, rel)
		if err != nil {
			g.leftOut(rel, err)
			continue
		}
		t := Target{Release: rel, Container: o.Name, IP: o.Labels[LabelEdgeIP], Routes: routes}
		for _, r := range routes {
			if _, seen := t.upstream(r.Service, r.Port); !seen {
				t.Ports = append(t.Ports, Port{Service: r.Service, Port: r.Port})
			}
		}
		// While a proxy replaces another, both are labeled: the gateway uses the one Expose named,
		// or else the newer one.
		i := slices.IndexFunc(out, func(x Target) bool { return x.Release == rel })
		switch {
		case i < 0:
			out = append(out, t)
		case proxy == o.Name || proxy != out[i].Container && revision(o) > revisions[out[i].Container]:
			out[i] = t
		}
		revisions[o.Name] = revision(o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release < out[j].Release })
	return out, nil
}

// Sync rewrites the gateway from the settings and every release's proxy, and creates or removes
// the gateway container as needed. It returns the targets it routes to.
func (g *Gateway) Sync(ctx context.Context) ([]Target, error) {
	unlock, err := Lock(ctx, g.home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return g.syncLocked(ctx, "")
}

// SyncLocked is Sync for a caller that holds the gateway lock (Lock).
func (g *Gateway) SyncLocked(ctx context.Context) ([]Target, error) {
	return g.syncLocked(ctx, "")
}

// Expose records what release rel exposes and syncs the gateway, checking again under the gateway
// lock that no other release took the same listeners, hosts or paths since the pre-flight.
// proxy is the release proxy container the gateway sends the release's traffic to. Undo puts back
// what the release exposed before and syncs again, for a gateway that did not route with it.
func (g *Gateway) Expose(ctx context.Context, rel, proxy string, rs []resource.Resource, routes []Route) (targets []Target, undo func(context.Context) error, err error) {
	unlock, err := Lock(ctx, g.home)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	// Preflight reported the warnings; this check runs again under the lock only for the errors.
	if _, err := g.Check(ctx, rel, rs); err != nil {
		return nil, nil, err
	}
	before, err := os.ReadFile(exposeFile(g.home, rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	undo = func(ctx context.Context) error {
		unlock, err := Lock(ctx, g.home)
		if err != nil {
			return err
		}
		defer unlock()
		if before == nil {
			err = os.Remove(exposeFile(g.home, rel))
		} else {
			err = writeFile(exposeFile(g.home, rel), before)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_, err = g.syncLocked(ctx, "")
		return err
	}
	if err := writeExpose(g.home, rel, proxy, routes); err != nil {
		return nil, nil, err
	}
	targets, err = g.syncLocked(ctx, rel)
	if err != nil {
		err = errors.Join(err, undo(context.WithoutCancel(ctx)))
	}
	return targets, undo, err
}

// gateways returns the gateway containers on the host (more than one while one replaces another).
func gateways(ctx context.Context, rt Runtime) ([]ports.Object, error) {
	objs, err := rt.ListLabeled(ctx, LabelComponent)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(objs, func(o ports.Object) bool { return o.Labels[LabelComponent] != "gateway" }), nil
}

// Current returns the gateway container: the one made from this server's settings if it exists,
// else any; ports.ErrNotFound when there is none.
func (g *Gateway) Current(ctx context.Context) (string, error) {
	gws, err := gateways(ctx, g.rt)
	if err != nil {
		return "", err
	}
	if len(gws) == 0 {
		return "", fmt.Errorf("gateway %w", ports.ErrNotFound)
	}
	want := request(g.host.Proxies, Dir(g.home)).Name
	if i := slices.IndexFunc(gws, func(o ports.Object) bool { return o.Name == want }); i >= 0 {
		return gws[i].Name, nil
	}
	return gws[0].Name, nil
}

func (g *Gateway) syncLocked(ctx context.Context, self string) ([]Target, error) {
	rt, home := g.rt, g.home
	targets, err := g.targetsOf(ctx, self)
	if err != nil {
		return nil, err
	}
	dir := Dir(home)
	exposed := slices.ContainsFunc(targets, func(t Target) bool { return len(t.Routes) > 0 })
	gws, err := gateways(ctx, rt)
	if err != nil {
		return nil, err
	}
	// The host has one gateway. Another KARGA_HOME's is never replaced nor removed: two servers
	// would replace each other's for ever (one karga-server per host).
	for _, o := range gws {
		if strings.HasPrefix(o.Labels[labelSource], dir+" ") {
			continue
		}
		if !exposed {
			return nil, os.RemoveAll(dir)
		}
		other, _, _ := strings.Cut(o.Labels[labelSource], " ")
		return nil, fmt.Errorf("%w: the host gateway %s is the one of the karga-server with its state in %s; one karga-server runs per host",
			ports.ErrConflict, o.Name, filepath.Dir(other))
	}
	if !exposed {
		for _, o := range gws {
			if err := proxyops.RemoveWorkload(ctx, rt, o.Name); err != nil && !errors.Is(err, ports.ErrNotFound) {
				return nil, err
			}
		}
		return nil, os.RemoveAll(dir)
	}
	settings, err := ReadSettings(home)
	if err != nil {
		return nil, err
	}
	if err := g.writeConfig(dir, settings, targets); err != nil {
		return nil, err
	}
	want := request(g.host.Proxies, dir)
	var old []string
	have := false
	for _, o := range gws {
		if o.Name == want.Name {
			have = true
		} else {
			old = append(old, o.Name)
		}
	}
	if !have {
		if err := g.create(ctx, want); err != nil {
			return nil, err
		}
	}
	if st, err := rt.GetWorkload(ctx, want.Name); err != nil || st.State != ports.StateRunning {
		if err := rt.StartWorkload(ctx, want.Name); err != nil {
			return targets, err
		}
	}
	if len(old) == 0 {
		return targets, nil
	}
	return targets, g.handOver(ctx, settings, targets, want.Name, have, old)
}

// targetsOf is what the gateway routes to. Another release's certificate that cannot be read leaves
// its routes out; self's fails.
func (g *Gateway) targetsOf(ctx context.Context, self string) ([]Target, error) {
	targets, err := g.findTargets(ctx)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		if err := readableCerts(targets[i]); err != nil {
			if targets[i].Release == self {
				return nil, err
			}
			g.leftOut(targets[i].Release, err)
			targets[i].Routes, targets[i].Ports = nil, nil
		}
	}
	return targets, nil
}

// writeConfig writes the gateway's configuration and certificates into dir. Certificates go first:
// a listener referencing a missing file would be rejected.
func (g *Gateway) writeConfig(dir string, settings Settings, targets []Target) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := syncCerts(dir, targets, false); err != nil {
		return err
	}
	cfg := buildConfig(settings, targets, g.host.Proxies.Container().MountPath)
	cfg.AccessLog = settings.AccessLog
	// The socket is created by root in the 0700 directory, which keeps other users out.
	if err := g.host.Proxies.Write(dir, cfg); err != nil {
		return err
	}
	return syncCerts(dir, targets, true)
}

// create creates the gateway container want, with its own Envoy instance of the gateway's directory.
func (g *Gateway) create(ctx context.Context, want ports.WorkloadRequest) error {
	if err := g.rt.EnsureImage(ctx, g.host.Proxies.Container().Image, "if-not-present", nil); err != nil {
		return err
	}
	if err := g.host.Proxies.WriteInstance(Dir(g.home), instance(want.Name), "gateway", 0o666); err != nil {
		return err
	}
	return g.rt.CreateWorkload(ctx, want)
}

// handOver moves traffic from the old gateways to name. Another image or limits (D38): the new
// gateway binds the same ports (SO_REUSEPORT) and takes traffic once it routes; the old one stops
// accepting, its connections finish, and it goes. A new gateway (not have) that does not get ready
// is removed.
func (g *Gateway) handOver(ctx context.Context, settings Settings, targets []Target, name string, have bool, old []string) error {
	rt, home := g.rt, g.home
	all := Want(settings, targets, func(t Target) bool { return len(t.Routes) > 0 })
	if err := g.host.Proxies.WaitReady(ctx, adminDir(home, name), all, proxyops.ProxyReadyTimeout); err != nil {
		if !have {
			err = errors.Join(err, removeGateway(ctx, rt, home, name))
		}
		return err
	}
	for _, o := range old {
		_, _ = g.host.Proxies.Drain(ctx, adminDir(home, o), false, proxyops.ProxyDrainTimeout) // a gone one is drained
		if err := removeGateway(ctx, rt, home, o); err != nil {
			return err
		}
	}
	return nil
}

// removeGateway deletes a gateway container and its instance directory.
func removeGateway(ctx context.Context, rt Runtime, home, name string) error {
	if err := proxyops.RemoveWorkload(ctx, rt, name); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return err
	}
	return os.RemoveAll(filepath.Join(Dir(home), instance(name)))
}

// Wait waits until the gateway routes to every exposed port of release rel ("": of every release).
func (g *Gateway) Wait(ctx context.Context, rel string, targets []Target) error {
	h, home := g.host, g.home
	settings, err := ReadSettings(home)
	if err != nil {
		return err
	}
	want := Want(settings, targets, func(t Target) bool { return (rel == "" || t.Release == rel) && len(t.Routes) > 0 })
	if len(want) == 0 {
		return nil
	}
	name, err := g.Current(ctx)
	if err != nil {
		return err
	}
	return h.Proxies.WaitReady(ctx, adminDir(home, name), want, proxyops.ProxyReadyTimeout)
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
	// Revision is the settings revision in force (0: the defaults, none applied).
	Revision int
	// Error says why the admin API could not be read, if it could not.
	Error string
}

// Health reads the gateway container and Envoy's admin API.
func (g *Gateway) Health(ctx context.Context) (State, error) {
	rt, h, home := g.rt, g.host, g.home
	settings, rev, err := RevisionSettings(home, 0)
	if err != nil {
		return State{}, err
	}
	st := State{AccessLog: settings.AccessLog, Listen: settings.Listen, Revision: rev}
	name, err := g.Current(ctx)
	var ws ports.WorkloadStatus
	if err == nil {
		ws, err = rt.GetWorkload(ctx, name)
	}
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
	if st.Ready, err = h.Proxies.Live(ctx, adminDir(home, name)); err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Version, _ = h.Proxies.Version(ctx, adminDir(home, name))
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
	home := g.home
	settings, err := ReadSettings(home)
	if err != nil {
		return nil, err
	}
	targets, err := g.findTargets(ctx)
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
				switch {
				case r.PathType == resource.PathExact:
					route += " (exact)"
				case r.To != "":
					route += " → " + r.To
				}
			}
			out = append(out, Exposed{Listener: fmt.Sprintf("%s:%d", l.Name, l.Port), Route: route, Release: t.Release,
				Service: fmt.Sprintf("%s:%d", r.Service, r.Port), Upstream: up})
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
	targets, err := g.findTargets(ctx)
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
