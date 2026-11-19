package deployment

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// fakeProxies writes real config files but fakes the admin API. Calls go into the runtime's call
// log, so ordering against container operations can be checked in one list:
// "Write:proxy|gateway:<endpoints>", "WaitReady:<kind>:<endpoints>", "WaitDrained:<kind>".
type fakeProxies struct {
	fakeruntime.Proxies
	rt       *fakeruntime.Runtime
	home     string
	notReady map[string]bool // by kind
}

func (f *fakeProxies) kind(dir string) string {
	if dir == gateway.Dir(f.home) || filepath.Dir(dir) == gateway.Dir(f.home) { // the gateway's, or an instance's
		return "gateway"
	}
	return "proxy"
}

func (f *fakeProxies) Drain(_ context.Context, dir string, _ bool, _ time.Duration) (bool, error) {
	f.log("Drain:" + f.kind(dir))
	return true, nil
}

func (f *fakeProxies) log(s string) { f.rt.Calls = append(f.rt.Calls, s) }

func endpoints(m map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return strings.Join(out, ",")
}

func (f *fakeProxies) Write(dir string, cfg ports.ProxyConfig) error {
	if err := envoy.Write(dir, cfg); err != nil {
		return err
	}
	eps, _ := testHost.Proxies.Endpoints(dir)
	f.log("Write:" + f.kind(dir) + ":" + endpoints(eps))
	return nil
}

func (f *fakeProxies) WaitReady(_ context.Context, dir string, want map[string]string, _ time.Duration) error {
	f.log("WaitReady:" + f.kind(dir) + ":" + endpoints(want))
	if f.notReady[f.kind(dir)] {
		return errors.New(f.kind(dir) + " not ready")
	}
	return nil
}

func (f *fakeProxies) WaitDrained(_ context.Context, dir string, _ []string, _ time.Duration) (bool, error) {
	f.log("WaitDrained:" + f.kind(dir))
	return true, nil
}

// newProxyRequest installs replaceManifest with Service redis exposed on tcp listener pg.
func newProxyRequest(t *testing.T) (*request, *fakeProxies) {
	t.Helper()
	withProxySupport(t, true)
	r := newRequest(t, freePort(t))
	if err := gateway.WriteSettings(r.home, gateway.Settings{Listeners: []gateway.Listener{{Name: "pg", Port: freePort(t), Protocol: gateway.ProtocolTCP}}}); err != nil {
		t.Fatal(err)
	}
	r.req.Resources = exposedManifest(r.port())
	px := &fakeProxies{rt: r.rt, home: r.home, notReady: map[string]bool{}}
	r.req.ProxyUser = "1000:1000"
	withProxies(t, px)
	return r, px
}

func exposedManifest(port int) []resource.Resource {
	rs := replaceManifest(port)
	rs[len(rs)-1].Spec.(*resource.ServiceSpec).Expose = []resource.Exposure{{Port: "p", Listener: "pg"}}
	return rs
}

func (r *request) port() int {
	return r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].ContainerPort
}

// upgrade runs revision rev of the exposed manifest with a new image.
func (r *request) upgrade(rev int) error {
	r.rt.Calls = nil
	r.req.Revision = rev
	r.req.Resources = exposedManifest(r.port())
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	return r.run()
}

func before(t *testing.T, calls []string, first, second string) {
	t.Helper()
	a, b := callIndex(calls, first), callIndex(calls, second)
	if a < 0 || b < 0 || a > b {
		t.Fatalf("%s must come before %s:\n%s", first, second, strings.Join(calls, "\n"))
	}
}

// upstream is where the gateway reaches the exposed port (the manifests' service port) of proxy
// container w: its edge address.
func upstream(t *testing.T, w *fakeruntime.Workload, port int) string {
	t.Helper()
	ip := w.Request.Labels[gateway.LabelEdgeIP]
	if ip == "" {
		t.Fatalf("edge label: %v", w.Request.Labels)
	}
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

func TestProxyAndGatewayInstall(t *testing.T) {
	r, _ := newProxyRequest(t)
	port := r.port()
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	w := r.rt.Workload("cache-redis")
	if !slices.Equal(w.Request.Aliases, []string{"karga-redis-r1"}) || len(w.Request.Ports) != 0 {
		t.Fatalf("the workload carries only the backend alias and publishes nothing: %+v", w.Request)
	}

	proxy := r.rt.Workload("cache-karga-proxy")
	pr := proxy.Request
	if !slices.Equal(pr.Aliases, []string{"redis"}) || !slices.Equal(pr.Networks, []string{"cache-net"}) || pr.User != "1000:1000" ||
		pr.Sysctls["net.ipv4.ip_unprivileged_port_start"] != "0" || pr.Mounts[0].Target != testHost.Proxies.Container().MountPath {
		t.Fatalf("proxy request: %+v", pr)
	}
	// It publishes nothing: it is on the edge network at the release's first address.
	if len(pr.Ports) != 0 || pr.Labels[gateway.LabelEdgeIP] != "10.213.0.2" || r.rt.IPs[proxy.Request.Name][gateway.EdgeNetwork] != "10.213.0.2" {
		t.Fatalf("the proxy's edge address: %+v %v %v", pr.Ports, pr.Labels, r.rt.IPs)
	}
	if n := r.rt.Networks[gateway.EdgeNetwork]; !n.Internal || n.Subnet != "10.213.0.0/16" {
		t.Fatalf("edge network: %+v", n)
	}
	for _, req := range []ports.WorkloadRequest{pr, r.rt.Workload(gateway.ContainerName).Request} {
		if req.MemoryBytes != 128<<20 || req.MilliCPU != 500 || req.PidsLimit != 256 {
			t.Fatalf("%s limits: mem %d cpu %d pids %d", req.Name, req.MemoryBytes, req.MilliCPU, req.PidsLimit)
		}
	}
	if w.Request.PidsLimit != 1024 {
		t.Fatalf("workload default pids: %d", w.Request.PidsLimit)
	}
	gw := r.rt.Workload(gateway.ContainerName).Request
	if !gw.HostNetwork || gw.User != "0:0" || !slices.Equal(gw.CapDrop, []string{"ALL"}) || !gw.NoNewPrivileges || gw.Labels[ports.LabelRelease] != "" {
		t.Fatalf("gateway request: %+v", gw)
	}

	backend := "karga-redis-r1:" + strconv.Itoa(port)
	c := r.rt.Calls
	before(t, c, "StartWorkload:cache-redis", "Write:proxy:"+backend)
	before(t, c, "Write:proxy:"+backend, "CreateWorkload:cache-karga-proxy")
	before(t, c, "StartWorkload:cache-karga-proxy", "WaitReady:proxy:karga-redis-r1")
	before(t, c, "WaitReady:proxy:karga-redis-r1", "Write:gateway:"+upstream(t, proxy, port))
	before(t, c, "Write:gateway:", "CreateWorkload:"+gateway.ContainerName)
	before(t, c, "StartWorkload:"+gateway.ContainerName, "WaitReady:gateway:"+upstream(t, proxy, port))
	cfgEps, _ := testHost.Proxies.Endpoints(gateway.Dir(r.home))
	if cfgEps[gateway.Cluster("cache", "redis", port)] != upstream(t, proxy, port) {
		t.Fatalf("gateway endpoints: %v", cfgEps)
	}
}

func TestProxyReplace(t *testing.T) {
	r, _ := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if err := r.upgrade(2); err != nil {
		t.Fatal(err)
	}
	c := r.rt.Calls
	for _, step := range [][2]string{
		{"StartWorkload:cache-redis", "Write:proxy:karga-redis-r2"},
		{"Write:proxy:karga-redis-r2", "WaitReady:proxy:karga-redis-r2"},
		{"WaitReady:proxy:karga-redis-r2", "WaitDrained:proxy"},
		{"WaitDrained:proxy", "StopWorkload:cache-redis"},
		{"StopWorkload:cache-redis", "DeleteWorkload:cache-redis"},
	} {
		before(t, c, step[0], step[1])
	}
	// The gateway forwards to the proxy, so a replace switches nothing there.
	if slices.ContainsFunc(c, func(s string) bool {
		return strings.HasSuffix(s, ":cache-karga-proxy") || strings.HasSuffix(s, ":"+gateway.ContainerName) || s == "WaitDrained:gateway"
	}) {
		t.Fatalf("a replace must not touch the proxy or gateway containers: %v", c)
	}
	if w := r.rt.Workload("cache-redis").Request; w.Image != "redis:7.2" || !slices.Equal(w.Aliases, []string{"karga-redis-r2"}) {
		t.Fatalf("after replace: %+v", w)
	}
}

// A changed proxy (here: another Service, so new aliases) starts next to the old one, at the other
// address of the release's pair; the gateway switches to it before the old one drains and goes.
func TestProxyReplacedNextToOld(t *testing.T) {
	r, _ := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	proxies := func() []string {
		var out []string
		for n, w := range r.rt.Workloads {
			if w.Request.Labels[ports.LabelKind] == KindProxy {
				out = append(out, n)
			}
		}
		return out
	}
	old := proxies()
	r.rt.Calls, r.rt.Raw = nil, nil
	r.req.Revision = 2
	r.req.Resources = withWorkload(exposedManifest(r.port()), "web", r.port()+1)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	now := proxies()
	if b := r.rt.Workloads[now[0]].Request.Labels[gateway.LabelEdgeIP]; b != "10.213.0.3" {
		t.Fatalf("the replacing proxy's address: %q", b)
	}
	if len(old) != 1 || len(now) != 1 || now[0] == old[0] {
		t.Fatalf("proxies before %v, after %v", old, now)
	}
	raw := r.rt.Raw
	before(t, raw, "StartWorkload:"+now[0], "DeleteWorkload:"+old[0])
	c := r.rt.Calls
	for _, step := range [][2]string{
		{"StartWorkload:cache-karga-proxy", "WaitReady:proxy:karga-redis-r1"},
		{"WaitReady:proxy:karga-redis-r1", "Write:gateway:" + upstream(t, r.rt.Workloads[now[0]], r.port())},
		{"Write:gateway:" + upstream(t, r.rt.Workloads[now[0]], r.port()), "Drain:proxy"},
		{"Drain:proxy", "DeleteWorkload:cache-karga-proxy"},
	} {
		before(t, c, step[0], step[1])
	}
	if slices.Contains(c, "StopWorkload:cache-redis") {
		t.Fatalf("the workload must not be touched: %v", c)
	}
	if data, err := os.ReadFile(filepath.Join(ProxyDir(r.home, "cache"), "expose.yaml")); err != nil || !strings.Contains(string(data), "proxy: "+now[0]+"\n") {
		t.Fatalf("the expose file must name %s: %s %v", now[0], data, err)
	}
	if _, err := os.Stat(ProxyAdminDir(r.home, "cache", old[0])); !os.IsNotExist(err) {
		t.Fatalf("the old proxy's instance directory is left: %v", err)
	}
}

func TestReplaceRevertsWhenProxyNotReady(t *testing.T) {
	r, px := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	px.notReady["proxy"] = true
	old := r.rt.Name("cache-redis")
	if err := r.upgrade(2); err == nil {
		t.Fatal("expected failure")
	}
	before(t, r.rt.Calls, "Write:proxy:karga-redis-r2", "Write:proxy:karga-redis-r1")
	if slices.Contains(r.rt.Raw, "StopWorkload:"+old) {
		t.Fatalf("old container stopped although the proxy never got ready: %v", r.rt.Raw)
	}
	if eps, _ := testHost.Proxies.Endpoints(ProxyDir(r.home, "cache")); eps[proxyCluster("redis", r.port())] != "karga-redis-r1:"+strconv.Itoa(r.port()) {
		t.Fatalf("old backend not restored: %v", eps)
	}
	for _, n := range r.rt.Containers("cache", "redis") {
		if n != old && r.rt.Workloads[n].State != ports.StateExited {
			t.Fatalf("the failed new container should be stopped and kept: %+v", r.rt.Workloads[n])
		}
	}
}

func TestGatewayRejectionIsReported(t *testing.T) {
	r, px := newProxyRequest(t)
	px.notReady["gateway"] = true
	r.rt.Output = map[string]string{gateway.ContainerName: "[2026-11-17][1][info][main] starting\n" +
		"[2026-11-17][1][warning][config] Filesystem config update rejected: cannot bind '0.0.0.0:80': Permission denied\n"}
	err := r.run()
	if err == nil || !strings.Contains(err.Error(), "cannot bind '0.0.0.0:80'") || strings.Contains(err.Error(), "starting") {
		t.Fatalf("want the gateway's rejection in the error, got %v", err)
	}
}

func TestGatewayGoesWithTheLastRelease(t *testing.T) {
	r, _ := newProxyRequest(t)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if err := envAt(r.home).Delete(context.Background(), r.rt, "cache", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.rt.Lookup(gateway.ContainerName); ok {
		t.Fatal("gateway still exists")
	}
	if eps, _ := testHost.Proxies.Endpoints(gateway.Dir(r.home)); len(eps) != 0 {
		t.Fatalf("gateway config left: %v", eps)
	}
}

func TestReconcileKeepsWhatTheProxyRoutesTo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	create := func(name, rev string) {
		t.Helper()
		lbl := map[string]string{ports.LabelManaged: "true", ports.LabelRelease: "web", ports.LabelKind: resource.KindWorkload,
			ports.LabelName: "app", ports.LabelRevision: rev, ports.LabelDigest: "sha256:" + name[len(name)-6:]}
		if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: name, Image: "img", Labels: lbl}); err != nil {
			t.Fatal(err)
		}
	}
	applied := []release.AppliedResource{{Kind: resource.KindWorkload, Name: "app", RuntimeName: "web-app-aaaaaa", Digest: "sha256:aaaaaa"}}
	// Interrupted after the proxy switched to revision 2 but before the old container went away.
	create("web-app-aaaaaa", "1")
	create("web-app-bbbbbb", "2")
	cfg := ports.ProxyConfig{Clusters: []ports.ProxyCluster{{Name: "app-p8080", Host: BackendAlias("app", 2), Port: 8080, DNS: true}}}
	if err := envoy.Write(ProxyDir(home, "web"), cfg); err != nil {
		t.Fatal(err)
	}
	repaired, err := envAt(home).RepairReplace(ctx, rt, "web", applied)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.Containers("web", "app"), []string{"web-app-bbbbbb"}) || len(repaired) != 1 || repaired[0].RuntimeName != "web-app-bbbbbb" {
		t.Fatalf("kept the wrong container: %v %v", repaired, rt.Containers("web", "app"))
	}
}
