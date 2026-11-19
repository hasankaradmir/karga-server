package deployment

import (
	"context"
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

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// revisionOf reads the revision label of a running workload, so the proxy is pointed at the alias
// the container actually carries.
// revisionOf returns the revision of the container a workload runs as, of names (see
// Plan.containers): the first running one, else the first that exists (a failed replace leaves its
// new container stopped next to the old one, which still serves).
func revisionOf(ctx context.Context, rt ports.Runtime, names []string) (int, error) {
	var found *ports.WorkloadStatus
	for _, name := range names {
		st, err := rt.GetWorkload(ctx, name)
		if err != nil {
			continue
		}
		if st.State == ports.StateRunning {
			return strconv.Atoi(st.Labels[ports.LabelRevision])
		}
		if found == nil {
			found = &st
		}
	}
	if found == nil {
		return 0, fmt.Errorf("no container of %v: %w", names, ports.ErrNotFound)
	}
	return strconv.Atoi(found.Labels[ports.LabelRevision])
}

// proxyWant is what the release proxy must route to for workload's services to reach it via alias.
func proxyWant(svcs services, workload, alias string) map[string]string {
	want := map[string]string{}
	for _, n := range svcs.targeting(workload) {
		for _, p := range svcs.svc[n].Ports {
			if _, ok := svcs.target(n, p); ok {
				want[proxyCluster(n, p.Port)] = alias // Envoy reports a DNS cluster's host by name
			}
		}
	}
	return want
}

// proxyClusters are the clusters of the services that send traffic to workload.
func proxyClusters(svcs services, workload string) []string {
	var out []string
	for _, n := range svcs.targeting(workload) {
		for _, p := range svcs.svc[n].Ports {
			out = append(out, proxyCluster(n, p.Port))
		}
	}
	return out
}

// proxyConfig listens on every service port and routes it to the backend alias of the container
// running the service's workload (override names another alias per workload, for a replace). A
// service whose workload has no container yet is left out until it has one.
func proxyConfig(ctx context.Context, rt ports.Runtime, plan Plan, svcs services, override map[string]string) ports.ProxyConfig {
	var cfg ports.ProxyConfig
	for _, n := range svcs.names() {
		s := svcs.svc[n]
		w := svcs.wl[s.Workload]
		if w == nil {
			continue
		}
		alias, ok := override[s.Workload]
		if !ok {
			rev, err := revisionOf(ctx, rt, plan.containers(s.Workload))
			if err != nil {
				continue
			}
			alias = BackendAlias(s.Workload, rev)
		}
		for _, p := range s.Ports {
			port, ok := svcs.target(n, p)
			if !ok {
				continue
			}
			cluster := proxyCluster(n, p.Port)
			cfg.TCP = append(cfg.TCP, ports.ProxyTCPListener{Address: "0.0.0.0", Port: p.Port, Cluster: cluster})
			cfg.Clusters = append(cfg.Clusters, ports.ProxyCluster{Name: cluster, Host: alias, Port: port, DNS: true,
				HealthPath: proxyops.HTTPHealthPath(w, port)})
		}
	}
	return cfg
}

func (e *Env) writeProxy(ctx context.Context, rt ports.Runtime, req ExecRequest, svcs services, override map[string]string) error {
	return e.host.Proxies.Write(ProxyDir(e.home, req.Release), req.Release, 0o600, proxyConfig(ctx, rt, req.Plan, svcs, override))
}

// exposeRoutes is what the release exposes on the gateway, written next to the proxy's
// configuration for the gateway to read.
func exposeRoutes(svcs services, paths map[Key]string) []gateway.Route {
	var out []gateway.Route
	for _, n := range svcs.names() {
		s := svcs.svc[n]
		for _, e := range s.Expose {
			p, ok := s.PortNamed(e.Port)
			if !ok {
				continue
			}
			r := gateway.Route{Service: n, Port: p.Port, Listener: e.Listener, Host: e.Host, Path: e.Path}
			if port, ok := svcs.target(n, p); ok {
				r.HealthPath = proxyops.HTTPHealthPath(svcs.wl[s.Workload], port)
			}
			if e.TLS != nil {
				r.TLSSecret, r.CertDir = e.TLS.Secret, paths[Key{resource.KindSecret, e.TLS.Secret}]
			}
			out = append(out, r)
		}
	}
	return out
}

// proxyRequest is the release's Envoy container. It runs as the karga user so it can read its 0700
// directory and karga can reach its admin socket. It joins its first network at creation; the
// others are connected with their own aliases (connectProxy).
func (e *Env) proxyRequest(rel, name string, revision int, spec ProxySpec, dir, user string) (ports.WorkloadRequest, error) {
	wr := ports.WorkloadRequest{
		Name:          name,
		Labels:        labels(rel, revision, KindProxy, ProxyResource),
		Image:         spec.Image,
		Command:       e.host.Proxies.Command(proxyInstance(rel, name)),
		User:          user,
		Sysctls:       map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
		RestartPolicy: "always",
		Mounts:        []ports.Mount{{Type: ports.MountBind, Source: dir, Target: e.host.Proxies.Container().MountPath}},
		MemoryBytes:   e.host.Proxies.Container().MemoryBytes,
		MilliCPU:      e.host.Proxies.Container().MilliCPU,
		PidsLimit:     e.host.Proxies.Container().Pids,
		LogMaxSize:    resource.DefaultLogMaxSize,
		LogMaxFiles:   resource.DefaultLogMaxFiles,
	}
	if nets := sortedNetworks(spec); len(nets) > 0 {
		wr.Networks, wr.Aliases = []string{proxyops.RuntimeName(rel, nets[0])}, spec.Networks[nets[0]]
	}
	return wr, gateway.Publish(&wr, spec.Published)
}

func sortedNetworks(spec ProxySpec) []string {
	var out []string
	for n := range spec.Networks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// createProxy writes the configuration for what runs now and starts the release proxy c names, next
// to the one it replaces if any (both serve the same files; each has its own admin socket). A
// container of that name left by an interrupted operation is removed first.
func (e *Env) createProxy(ctx context.Context, rt ports.Runtime, req ExecRequest, c Change, svcs services, owned map[string]bool) error {
	if err := e.writeProxy(ctx, rt, req, svcs, nil); err != nil {
		return err
	}
	dir := ProxyDir(e.home, req.Release)
	if inst := proxyInstance(req.Release, c.RuntimeName); inst != "" {
		if err := e.host.Proxies.WriteInstance(dir, inst, req.Release, 0o600); err != nil {
			return err
		}
	}
	if owned[ports.ObjectWorkload+"/"+c.RuntimeName] && (c.Applied == nil || c.Applied.RuntimeName != c.RuntimeName) {
		if err := proxyops.RemoveWorkload(ctx, rt, c.RuntimeName); err != nil {
			return err
		}
	}
	wr, err := e.proxyRequest(req.Release, c.RuntimeName, req.Revision, *c.Proxy, dir, req.ProxyUser)
	if err != nil {
		return err
	}
	wr.Labels[ports.LabelDigest] = c.Digest
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		return err
	}
	for _, n := range sortedNetworks(*c.Proxy)[1:] {
		if err := rt.ConnectNetwork(ctx, wr.Name, proxyops.RuntimeName(req.Release, n), c.Proxy.Networks[n]); err != nil {
			return err
		}
	}
	return rt.StartWorkload(ctx, wr.Name)
}

// removeProxy deletes a proxy container; with dropConfig its directory goes too. A per-workload
// proxy of an earlier karga version had its own directory below the release's.
func removeProxy(ctx context.Context, rt ports.Runtime, home, rel string, c Change, dropConfig bool) error {
	if err := removeProxyContainer(ctx, rt, home, rel, c.Applied.RuntimeName); err != nil {
		return err
	}
	if !dropConfig {
		return nil
	}
	if c.Name != ProxyResource {
		return os.RemoveAll(filepath.Join(ProxyDir(home, rel), c.Name))
	}
	for _, f := range []string{"envoy.yaml", "lds.yaml", "cds.yaml", "admin.sock", "expose.yaml"} {
		if err := os.Remove(filepath.Join(ProxyDir(home, rel), f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// removeProxyContainer deletes a release proxy container and its instance directory.
func removeProxyContainer(ctx context.Context, rt ports.Runtime, home, rel, name string) error {
	if err := proxyops.RemoveWorkload(ctx, rt, name); err != nil {
		return err
	}
	if inst := proxyInstance(rel, name); inst != "" {
		return os.RemoveAll(filepath.Join(ProxyDir(home, rel), inst))
	}
	return nil
}

// gatewayRoutesTo reports whether the host gateway sends traffic to proxy container o (to one of
// the upstream ports it publishes): then an interrupted proxy replace switched over to it.
func (e *Env) gatewayRoutesTo(o ports.Object) bool {
	eps, _ := e.host.Proxies.Endpoints(gateway.Dir(e.home))
	for _, p := range gateway.ParseLabel(o.Labels) {
		if slices.Contains(slices.Collect(maps.Values(eps)), net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Upstream))) {
			return true
		}
	}
	return false
}

// routedTo reports whether the release proxy routes workload name to container next: then an
// interrupted replace switched over and next is the one to keep.
func (e *Env) routedTo(ctx context.Context, rt ports.Runtime, rel, name, next string) bool {
	st, err := rt.GetWorkload(ctx, next)
	if err != nil {
		return false
	}
	rev, err := strconv.Atoi(st.Labels[ports.LabelRevision])
	if err != nil {
		return false
	}
	eps, _ := e.host.Proxies.Endpoints(ProxyDir(e.home, rel))
	for _, ep := range eps {
		if strings.HasPrefix(ep, BackendAlias(name, rev)+":") {
			return true
		}
	}
	return false
}
