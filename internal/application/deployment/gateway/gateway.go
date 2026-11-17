// Package gateway runs the host gateway (ADR-011): one Envoy in the host network namespace that
// serves the listeners the host's administrator defines and forwards to the release proxies'
// loopback upstream ports. Packages only attach Services to listeners; they cannot open one.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment/runtimeops"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
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

// Dir holds the gateway's configuration and admin socket.
func Dir(home string) string { return filepath.Join(home, "gateway") }

// ExposeFile lists what a release exposes ([]Route). It lives in the release proxy's directory, so
// changing a host or path rewrites a file instead of recreating the proxy container.
func ExposeFile(home, rel string) string { return filepath.Join(home, "proxies", rel, "expose.json") }

// Port is a service port a release proxy publishes on a loopback upstream port for the gateway.
type Port struct {
	Service  string `json:"service"`
	Port     int    `json:"port"`
	Upstream int    `json:"upstream"`
}

// Route attaches a service port to a gateway listener: by Host and Path (a prefix) on an http or
// https listener, or alone on a tcp one.
type Route struct {
	Service  string `json:"service"`
	Port     int    `json:"port"`
	Listener string `json:"listener"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
	// TLSSecret and CertDir (its materialized directory, with tls.crt and tls.key) serve HTTPS.
	TLSSecret string `json:"tlsSecret,omitempty"`
	CertDir   string `json:"certDir,omitempty"`
	// HealthPath is checked by HTTP instead of a TCP connect (the workload's http health check).
	HealthPath string `json:"healthPath,omitempty"`
}

// allocatePort picks a free loopback port for an upstream. Docker binds it right after; a race
// with another process makes the container start fail, which is reported as usual.
func allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate an upstream port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Cluster names the cluster of one service port. Release and service names are DNS labels, so
// "_" cannot occur in them.
func Cluster(rel, service string, port int) string {
	return fmt.Sprintf("%s_%s_p%d", rel, service, port)
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

// ParseLabel reads the upstream ports of a proxy container.
func ParseLabel(labels map[string]string) []Port {
	var out []Port
	_ = json.Unmarshal([]byte(labels[LabelPorts]), &out) // karga writes it; a broken label means no ports
	return out
}

// ReadExpose reads what a release exposes; a release without the file exposes nothing.
func ReadExpose(home, rel string) ([]Route, error) {
	data, err := os.ReadFile(ExposeFile(home, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Route
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", ExposeFile(home, rel), err)
	}
	return out, nil
}

// WriteExpose stores what a release exposes (none removes the file).
func WriteExpose(home, rel string, routes []Route) error {
	if len(routes) == 0 {
		if err := os.Remove(ExposeFile(home, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	js, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ExposeFile(home, rel)), 0o700); err != nil {
		return err
	}
	return writeFile(ExposeFile(home, rel), js)
}

// findTargets lists every release proxy that publishes upstream ports, with its release's routes.
func findTargets(ctx context.Context, rt ports.Runtime, home string) ([]Target, error) {
	objs, err := rt.ListLabeled(ctx, LabelPorts)
	if err != nil {
		return nil, err
	}
	var out []Target
	for _, o := range objs {
		rel := o.Labels[ports.LabelRelease]
		routes, err := ReadExpose(home, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, Target{Release: rel, Container: o.Name, Ports: ParseLabel(o.Labels), Routes: routes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release < out[j].Release })
	return out, nil
}

// certName is the file name (without extension) of a TLS host's certificate in the gateway
// directory. It includes a digest of the directory the certificate comes from: Envoy reads
// certificate files only when the listener changes, so a new certificate needs a new name.
func certName(rel, listener, host, src string) string {
	sum := sha256.Sum256([]byte(src))
	return fmt.Sprintf("%s_%s_%s_%x", rel, listener, host, sum[:4])
}

// use is what the targets attach to one listener.
type use struct {
	tcp    string // cluster
	routes []envoy.HTTPRoute
	certs  map[string]envoy.Cert
	seen   map[string]bool
}

// buildConfig builds the gateway's listeners and clusters: every listener in use, on every listen
// address. A listener nobody uses is not opened. Conflicts are refused by Check; if one slips
// through anyway (two releases applied at once) the first release in name order wins. A host
// served on an https listener also gets a redirect on the http listeners of port 80.
func buildConfig(settings Settings, targets []Target) envoy.Config {
	var cfg envoy.Config
	uses := map[string]*use{}
	for _, t := range targets {
		for _, p := range t.Ports {
			cfg.Clusters = append(cfg.Clusters, envoy.Cluster{Name: Cluster(t.Release, p.Service, p.Port), Host: "127.0.0.1", Port: p.Upstream,
				HealthPath: healthPath(t.Routes, p)})
		}
		for _, r := range t.Routes {
			l, ok := settings.Listener(r.Listener)
			if _, published := t.upstream(r.Service, r.Port); !ok || !published {
				continue
			}
			u := uses[l.Name]
			if u == nil {
				u = &use{certs: map[string]envoy.Cert{}, seen: map[string]bool{}}
				uses[l.Name] = u
			}
			cluster := Cluster(t.Release, r.Service, r.Port)
			switch {
			case l.Protocol == ProtocolTCP:
				if u.tcp == "" {
					u.tcp = cluster
				}
			case !u.seen[r.Host+r.Path]:
				u.seen[r.Host+r.Path] = true
				u.routes = append(u.routes, envoy.HTTPRoute{Host: r.Host, Path: r.Path, Cluster: cluster})
				if _, ok := u.certs[r.Host]; l.Protocol == ProtocolHTTPS && !ok {
					n := envoy.MountPath + "/certs/" + certName(t.Release, l.Name, r.Host, r.CertDir)
					u.certs[r.Host] = envoy.Cert{Chain: n + ".crt", Key: n + ".key"}
				}
			}
		}
	}
	for _, l := range settings.Listeners {
		u := uses[l.Name]
		var redirects []string
		redirectPort := 0
		if l.Protocol == ProtocolHTTP && l.Port == 80 {
			redirects, redirectPort = redirectHosts(settings, uses, u)
		}
		if u == nil && len(redirects) == 0 {
			continue
		}
		for _, ip := range settings.Listen {
			if l.Protocol == ProtocolTCP {
				cfg.TCP = append(cfg.TCP, envoy.TCPListener{Address: ip, Port: l.Port, Cluster: u.tcp})
			} else {
				hl := envoy.HTTPListener{Address: ip, Port: l.Port, Redirects: redirects, RedirectPort: redirectPort}
				if u != nil {
					hl.Routes = u.routes
					if l.Protocol == ProtocolHTTPS {
						hl.Certs = u.certs
					}
				}
				cfg.HTTP = append(cfg.HTTP, hl)
			}
		}
	}
	return cfg
}

// redirectHosts returns the hosts served on an https listener but not on the http listener u, and the
// port of the first https listener, so plain HTTP on port 80 redirects to HTTPS.
func redirectHosts(s Settings, uses map[string]*use, u *use) ([]string, int) {
	var hosts []string
	port := 0
	for _, l := range s.Listeners {
		su := uses[l.Name]
		if l.Protocol != ProtocolHTTPS || su == nil {
			continue
		}
		for _, h := range slices.Sorted(maps.Keys(su.certs)) {
			if u != nil && slices.ContainsFunc(u.routes, func(r envoy.HTTPRoute) bool { return r.Host == h }) || slices.Contains(hosts, h) {
				continue
			}
			hosts = append(hosts, h)
			if port == 0 {
				port = l.Port
			}
		}
	}
	return hosts, port
}

func healthPath(routes []Route, p Port) string {
	for _, r := range routes {
		if r.Service == p.Service && r.Port == p.Port && r.HealthPath != "" {
			return r.HealthPath
		}
	}
	return ""
}

// syncCerts copies the certificates of the targets' HTTPS routes into the gateway directory and
// removes the ones no longer used. Files are 0600; the gateway reads them as root.
func syncCerts(dir string, targets []Target, prune bool) error {
	certs := filepath.Join(dir, "certs")
	want := map[string]bool{}
	for _, t := range targets {
		for _, r := range t.Routes {
			if r.TLSSecret == "" {
				continue
			}
			name := certName(t.Release, r.Listener, r.Host, r.CertDir)
			for ext, key := range map[string]string{".crt": resource.TLSCertKey, ".key": resource.TLSKeyKey} {
				data, err := os.ReadFile(filepath.Join(r.CertDir, key))
				if err != nil {
					return fmt.Errorf("%s/%s: certificate for %s: %w", t.Release, r.Service, r.Host, err)
				}
				if err := os.MkdirAll(certs, 0o700); err != nil {
					return err
				}
				if err := writeFile(filepath.Join(certs, name+ext), data); err != nil {
					return err
				}
				want[name+ext] = true
			}
		}
	}
	if !prune {
		return nil
	}
	entries, err := os.ReadDir(certs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !want[e.Name()] {
			if err := os.Remove(filepath.Join(certs, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeFile replaces path atomically with mode 0600, leaving an identical file alone.
func writeFile(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Want is what the gateway must route to for the given targets to be served: cluster -> endpoint.
func Want(targets []Target, keep func(Target) bool) map[string]string {
	want := map[string]string{}
	for _, t := range targets {
		if !keep(t) {
			continue
		}
		for _, p := range t.Ports {
			want[Cluster(t.Release, p.Service, p.Port)] = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Upstream))
		}
	}
	return want
}

// Lock serializes gateway updates of concurrent karga processes (release locks are per release).
func Lock(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock the gateway: %w", err)
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}

// labelSource records what a gateway container was created from; another KARGA_HOME or image
// means it must be recreated.
const labelSource = "io.karga.gateway-source"

func source(dir string) string { return dir + " " + runtimeops.EnvoyLimits + " " + envoy.Image() }

func request(dir string) ports.WorkloadRequest {
	return ports.WorkloadRequest{
		Name:   ContainerName,
		Labels: map[string]string{ports.LabelManaged: "true", LabelComponent: "gateway", labelSource: source(dir)},
		Image:  envoy.Image(),
		// Root only to bind low ports in the host namespace (Docker grants no ambient capabilities
		// to other users); every capability but those two is dropped.
		Command:         envoy.Command(),
		User:            "0:0",
		Env:             map[string]string{"ENVOY_UID": "0"},
		HostNetwork:     true,
		CapDrop:         []string{"ALL"},
		CapAdd:          []string{"NET_BIND_SERVICE", "DAC_OVERRIDE"},
		NoNewPrivileges: true,
		RestartPolicy:   "always",
		Mounts:          []ports.Mount{{Type: ports.MountBind, Source: dir, Target: envoy.MountPath}},
		MemoryBytes:     runtimeops.EnvoyMemory,
		MilliCPU:        runtimeops.EnvoyMilliCPU,
		PidsLimit:       runtimeops.EnvoyPids,
		LogMaxSize:      resource.DefaultLogMaxSize,
		LogMaxFiles:     resource.DefaultLogMaxFiles,
	}
}

// Sync rewrites the gateway from the settings and every release's proxy, and creates or removes
// the gateway container as needed. It returns the targets it routes to.
func Sync(ctx context.Context, rt ports.Runtime, px runtimeops.ProxyControl, home string) ([]Target, error) {
	unlock, err := Lock(home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return nil, err
	}
	dir := Dir(home)
	exposed := slices.ContainsFunc(targets, func(t Target) bool { return len(t.Routes) > 0 })
	if !exposed {
		if err := runtimeops.RemoveWorkload(ctx, rt, ContainerName); err != nil && !errors.Is(err, ports.ErrNotFound) {
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
	cfg := buildConfig(settings, targets)
	cfg.AccessLog = settings.AccessLog
	// The socket is created by root in the 0700 directory, which keeps other users out.
	if err := px.Write(dir, "gateway", 0o666, cfg); err != nil {
		return nil, err
	}
	if err := syncCerts(dir, targets, true); err != nil {
		return nil, err
	}
	st, err := rt.GetWorkload(ctx, ContainerName)
	if err == nil && st.Labels[labelSource] != source(dir) {
		if err := runtimeops.RemoveWorkload(ctx, rt, ContainerName); err != nil {
			return nil, err
		}
		err = fmt.Errorf("gateway %w", ports.ErrNotFound)
	}
	switch {
	case errors.Is(err, ports.ErrNotFound):
		if err := rt.EnsureImage(ctx, envoy.Image(), "if-not-present"); err != nil {
			return nil, err
		}
		if err := rt.CreateWorkload(ctx, request(dir)); err != nil {
			return nil, err
		}
		err = rt.StartWorkload(ctx, ContainerName)
	case err == nil && st.State != ports.StateRunning:
		err = rt.StartWorkload(ctx, ContainerName)
	}
	return targets, err
}

// Wait waits until the gateway routes to every exposed port of release rel.
func Wait(ctx context.Context, px runtimeops.ProxyControl, home, rel string, targets []Target) error {
	want := Want(targets, func(t Target) bool { return t.Release == rel && len(t.Routes) > 0 })
	if len(want) == 0 {
		return nil
	}
	return px.WaitReady(ctx, Dir(home), want, runtimeops.ProxyReadyTimeout)
}

// Listener protocols.
const (
	ProtocolTCP   = api.ProtocolTCP
	ProtocolHTTP  = api.ProtocolHTTP
	ProtocolHTTPS = api.ProtocolHTTPS
)

// Listener is shared with the CLI (karga-sdk/api).
type Listener = api.Listener

// Settings is shared with the CLI (karga-sdk/api).
type Settings = api.Settings

// DefaultListen is where the gateway listens until the administrator sets addresses.
const DefaultListen = api.DefaultListen

// DefaultListeners are the listeners a new host starts with.
var DefaultListeners = api.DefaultListeners

func settingsPath(home string) string { return filepath.Join(home, "gateway.json") }

// ReadSettings returns the stored settings, with defaults for what was never set.
func ReadSettings(home string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(settingsPath(home))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal(data, &s); err != nil {
			return s, fmt.Errorf("%s: %w", settingsPath(home), err)
		}
	}
	if len(s.Listen) == 0 {
		s.Listen = []string{DefaultListen}
	}
	if s.Listeners == nil { // never set (an empty list is the administrator's choice)
		s.Listeners = DefaultListeners()
	}
	return s, nil
}

// WriteSettings validates and stores settings; the next Sync applies them.
func WriteSettings(home string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.Listeners == nil {
		s.Listeners = []Listener{}
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return writeFile(settingsPath(home), data)
}

// State is shared with the CLI (karga-sdk/api).
type State = api.State

// Health reads the gateway container and Envoy's admin API.
func Health(ctx context.Context, rt ports.Runtime, home string) (State, error) {
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
	admin := envoy.NewAdmin(Dir(home))
	if st.Ready, err = admin.Live(ctx); err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Version, _ = admin.Version(ctx)
	return st, nil
}

// Exposed is shared with the CLI (karga-sdk/api).
type Exposed = api.Exposed

// Routes lists what the gateway serves, as computed from the release proxies.
func Routes(ctx context.Context, rt ports.Runtime, home string) ([]Exposed, error) {
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
			l, defined := settings.Listener(r.Listener)
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
func Users(ctx context.Context, rt ports.Runtime, home string) (map[string][]string, error) {
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

// Check refuses exposures of release rel that the gateway cannot serve: an unknown listener, a
// listener of another protocol, or a tcp listener, host and path or certificate another release
// already holds. It binds a listener nobody uses yet once, to see that no other process holds it.
func Check(ctx context.Context, rt ports.Runtime, home, rel string, rs []resource.Resource) []error {
	settings, err := ReadSettings(home)
	if err != nil {
		return []error{err}
	}
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return []error{err}
	}
	type owner struct{ release, service string }
	tcp := map[string]owner{}    // listener -> owner
	routes := map[string]owner{} // listener host+path -> owner
	certs := map[string]string{} // https host -> release/secret
	inUse := map[string]bool{}   // listeners the gateway serves already
	for _, t := range targets {
		for _, r := range t.Routes {
			inUse[r.Listener] = true
			if t.Release == rel {
				continue
			}
			o := owner{t.Release, r.Service}
			if r.Host == "" {
				tcp[r.Listener] = o
			} else {
				routes[r.Listener+" "+r.Host+r.Path] = o
			}
			if r.TLSSecret != "" {
				certs[r.Host] = t.Release + "/" + r.TLSSecret
			}
		}
	}
	var errs []error
	tested := map[string]bool{}
	for _, r := range rs {
		s, ok := r.Spec.(*resource.ServiceSpec)
		if !ok {
			continue
		}
		for _, e := range s.Expose {
			where := fmt.Sprintf("%s %q: expose %s on listener %s", resource.KindService, r.Metadata.Name, e.Port, e.Listener)
			l, ok := settings.Listener(e.Listener)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: no such gateway listener; the host's administrator adds it with 'karga gateway listener add %s --port <port> --protocol tcp|http|https'", where, e.Listener))
				continue
			}
			switch {
			case l.Protocol == ProtocolTCP && e.Host != "":
				errs = append(errs, fmt.Errorf("%s: %s is a tcp listener and takes no host, path or tls", where, l.Name))
				continue
			case l.Protocol != ProtocolTCP && e.Host == "":
				errs = append(errs, fmt.Errorf("%s: %s is an %s listener and needs a host", where, l.Name, l.Protocol))
				continue
			case l.Protocol == ProtocolHTTPS && e.TLS == nil:
				errs = append(errs, fmt.Errorf("%s: %s is an https listener and needs tls.secret", where, l.Name))
				continue
			case l.Protocol == ProtocolHTTP && e.TLS != nil:
				errs = append(errs, fmt.Errorf("%s: %s is a plain http listener; use an https listener for tls", where, l.Name))
				continue
			}
			if o, taken := tcp[l.Name]; taken {
				errs = append(errs, fmt.Errorf("%s: the listener is used by %s/%s: %w", where, o.release, o.service, ports.ErrPortInUse))
			}
			if o, taken := routes[l.Name+" "+e.Host+e.Path]; taken && e.Host != "" {
				errs = append(errs, fmt.Errorf("%s: %s%s is already routed to %s/%s: %w", where, e.Host, e.Path, o.release, o.service, ports.ErrPortInUse))
			}
			if e.TLS != nil && certs[e.Host] != "" && certs[e.Host] != rel+"/"+e.TLS.Secret {
				errs = append(errs, fmt.Errorf("%s: %s already serves HTTPS for %s with another certificate: %w", where, strings.SplitN(certs[e.Host], "/", 2)[0], e.Host, ports.ErrPortInUse))
			}
			if !inUse[l.Name] && !tested[l.Name] {
				tested[l.Name] = true
				for _, ip := range settings.Listen {
					if runtimeops.PortTaken("tcp", ip, l.Port) {
						errs = append(errs, fmt.Errorf("%s: %s:%d is taken by another process on this host: %w", where, ip, l.Port, ports.ErrPortInUse))
					}
				}
			}
		}
	}
	return errs
}

// Publish publishes service ports of a release proxy request on fresh loopback upstream ports and
// records them in its labels.
func Publish(wr *ports.WorkloadRequest, published map[string][]int) error {
	var label []Port
	for _, svc := range slices.Sorted(maps.Keys(published)) {
		for _, port := range published[svc] {
			up, err := allocatePort()
			if err != nil {
				return err
			}
			wr.Ports = append(wr.Ports, ports.PortBinding{ContainerPort: port, HostPort: up, HostIP: "127.0.0.1", Protocol: "tcp"})
			label = append(label, Port{Service: svc, Port: port, Upstream: up})
		}
	}
	if len(label) == 0 {
		return nil
	}
	js, err := json.Marshal(label)
	if err != nil {
		return err
	}
	wr.Labels = maps.Clone(wr.Labels)
	wr.Labels[LabelPorts] = string(js)
	return nil
}
