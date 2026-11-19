package gateway

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func testSettings() Settings {
	return Settings{Listen: []string{"127.0.0.1", "10.0.0.5"}, Listeners: append(defaultListeners(),
		Listener{Name: "pg", Port: 5432, Protocol: ProtocolTCP}, Listener{Name: "unused", Port: 8080, Protocol: ProtocolHTTP})}
}

func TestGatewayConfigListenersAndRedirects(t *testing.T) {
	targets := []Target{
		{Release: "a", IP: "10.213.0.2", Ports: []Port{{Service: "shop", Port: 80}, {Service: "db", Port: 5432}},
			Routes: []Route{
				{Service: "shop", Port: 80, Listener: "https", Host: "secure.example", Path: "/", TLSSecret: "cert", CertDir: "/c"},
				{Service: "shop", Port: 80, Listener: "http", Host: "plain.example", Path: "/"},
				{Service: "db", Port: 5432, Listener: "pg"},
			}},
		{Release: "b", IP: "10.213.0.4", Ports: []Port{{Service: "web", Port: 80}},
			Routes: []Route{
				{Service: "web", Port: 80, Listener: "https", Host: "both.example", Path: "/", TLSSecret: "cert", CertDir: "/d"},
				{Service: "web", Port: 80, Listener: "http", Host: "both.example", Path: "/"},
			}},
	}
	cfg := buildConfig(testSettings(), targets, "/etc/karga-proxy")
	if len(cfg.Clusters) != 3 || cfg.Clusters[0].Name != "a_shop_p80" || cfg.Clusters[0].Host != "10.213.0.2" || cfg.Clusters[0].Port != 80 {
		t.Fatalf("clusters: %+v", cfg.Clusters)
	}
	if len(cfg.TCP) != 2 || cfg.TCP[0].Port != 5432 || cfg.TCP[0].Cluster != "a_db_p5432" || cfg.TCP[1].Address != "10.0.0.5" {
		t.Fatalf("tcp listeners on every listen address: %+v", cfg.TCP)
	}
	var tls, plain []ports.ProxyHTTPListener
	for _, l := range cfg.HTTP {
		switch l.Port {
		case 443:
			tls = append(tls, l)
		case 80:
			plain = append(plain, l)
		default:
			t.Fatalf("a listener nobody uses was opened: %+v", l)
		}
	}
	if len(tls) != 2 || len(plain) != 2 {
		t.Fatalf("http listeners: %+v", cfg.HTTP)
	}
	if c := tls[0].Certs; len(c) != 2 || !strings.HasPrefix(c["secure.example"].Chain, "/etc/karga-proxy/certs/a_https_secure.example_") {
		t.Fatalf("certs: %+v", c)
	}
	// secure.example gets a redirect; both.example is served plainly on 80, so it does not.
	if r := plain[0].Redirects; !maps.Equal(r, map[string]int{"secure.example": 443}) {
		t.Fatalf("redirects: %v", r)
	}
}

func TestSyncCertsCopiesAndPrunes(t *testing.T) {
	src := t.TempDir()
	for k, v := range map[string]string{resource.TLSCertKey: "CERT", resource.TLSKeyKey: "KEY"} {
		if err := os.WriteFile(filepath.Join(src, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	targets := []Target{{Release: "a", Routes: []Route{{Service: "web", Port: 80, Listener: "https", Host: "secure.example", Path: "/", TLSSecret: "cert", CertDir: src}}}}
	if err := syncCerts(dir, targets, true); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "certs", certName("a", "https", "secure.example", src)+".key")
	if b, err := os.ReadFile(key); err != nil || string(b) != "KEY" {
		t.Fatalf("key: %q %v", b, err)
	}
	if info, _ := os.Stat(key); info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v", info.Mode())
	}
	if err := syncCerts(dir, nil, true); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "certs")); len(entries) != 0 {
		t.Fatalf("unused certificates kept: %v", entries)
	}
}

// proxyOf creates the proxy container of release rel with its upstream ports and expose file.
func proxyOf(t *testing.T, rt *fakeruntime.Runtime, home, rel string, ps []Port, routes []Route) {
	t.Helper()
	_ = ps // the ports are the routes'; the proxy serves them at its edge address
	if err := rt.CreateWorkload(context.Background(), ports.WorkloadRequest{Name: rel + "-karga-proxy-0a1b2c3d", Image: "img", Labels: map[string]string{
		ports.LabelManaged: "true", ports.LabelRelease: rel, ports.LabelName: "proxy", LabelEdgeIP: "10.213.0." + strconv.Itoa(len(rel)+10)}}); err != nil {
		t.Fatal(err)
	}
	if err := writeExpose(home, rel, "", routes); err != nil {
		t.Fatal(err)
	}
}

func TestCheckExposures(t *testing.T) {
	h := testHost(true)
	ctx := context.Background()
	home := t.TempDir()
	if err := WriteSettings(home, Settings{Listeners: append(defaultListeners(), Listener{Name: "pg", Port: 5432, Protocol: ProtocolTCP})}); err != nil {
		t.Fatal(err)
	}
	rt := fakeruntime.New("img")
	proxyOf(t, rt, home, "other", []Port{{Service: "web", Port: 80}, {Service: "db", Port: 5432}}, []Route{
		{Service: "web", Port: 80, Listener: "https", Host: "secure.example", Path: "/", TLSSecret: "cert"},
		{Service: "db", Port: 5432, Listener: "pg"},
	})
	check := func(e resource.Exposure) string {
		svc := resource.Resource{Kind: resource.KindService, Metadata: resource.Metadata{Name: "s"}, Spec: &resource.ServiceSpec{
			Workload: "w", Ports: []resource.ServicePort{{Name: "p", Port: 80, TargetPort: "p"}}, Expose: []resource.Exposure{e}}}
		_, err := New(rt, h, home).Check(ctx, "mine", []resource.Resource{svc}) // warnings: ports a test cannot bind
		if err == nil {
			return ""
		}
		return err.Error()
	}
	tls := &resource.ExposureTLS{Secret: "mycert"}
	for _, tt := range []struct {
		e    resource.Exposure
		want string
	}{
		{resource.Exposure{Port: "p", Listener: "nope", Host: "a.example", Path: "/"}, "no such gateway listener"},
		{resource.Exposure{Port: "p", Listener: "pg", Host: "a.example", Path: "/"}, "tcp listener and takes no host"},
		{resource.Exposure{Port: "p", Listener: "http"}, "needs a host"},
		{resource.Exposure{Port: "p", Listener: "https", Host: "a.example", Path: "/"}, "needs tls.secret"},
		{resource.Exposure{Port: "p", Listener: "http", Host: "a.example", Path: "/", TLS: tls}, "plain http listener"},
		{resource.Exposure{Port: "p", Listener: "pg"}, "used by other/db"},
		{resource.Exposure{Port: "p", Listener: "https", Host: "secure.example", Path: "/", TLS: &resource.ExposureTLS{Secret: "cert"}}, "already routed to other/web"},
		{resource.Exposure{Port: "p", Listener: "https", Host: "secure.example", Path: "/api", TLS: tls}, "another certificate"},
		{resource.Exposure{Port: "p", Listener: "https", Host: "mine.example", Path: "/", TLS: tls}, ""},
	} {
		if got := check(tt.e); tt.want == "" && got != "" || !strings.Contains(got, tt.want) {
			t.Errorf("%+v: want %q, got %q", tt.e, tt.want, got)
		}
	}
}

func TestSettings(t *testing.T) {
	home := t.TempDir()
	s, err := ReadSettings(home)
	if err != nil || !slices.Equal(s.Listen, []string{"127.0.0.1"}) || !slices.Equal(s.Listeners, defaultListeners()) {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s.Listeners = append(s.Listeners, Listener{Name: "alt", Port: 80, Protocol: ProtocolHTTP})
	if err := WriteSettings(home, s); err == nil || !strings.Contains(err.Error(), "port 80 is used by listener \"http\"") {
		t.Fatalf("duplicate port: %v", err)
	}
	if err := WriteSettings(home, Settings{Listen: []string{"localhost"}}); err == nil || !strings.Contains(err.Error(), "not an IP address") {
		t.Fatalf("bad address: %v", err)
	}
	// Removing every listener is the administrator's choice, not a reason to bring the defaults back.
	if err := WriteSettings(home, Settings{Listen: []string{"0.0.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadSettings(home); err != nil || len(s.Listeners) != 0 || s.Listen[0] != "0.0.0.0" {
		t.Fatalf("stored: %+v %v", s, err)
	}
}

func TestSyncFollowsExposures(t *testing.T) {
	h := testHost(true)
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img", envoy.Image())
	proxyOf(t, rt, home, "a", nil, nil)
	if _, err := New(rt, h, home).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.Lookup(ContainerName); ok {
		t.Fatal("gateway started with nothing exposed")
	}
	proxyOf(t, rt, home, "b", []Port{{Service: "web", Port: 80}}, []Route{{Service: "web", Port: 80, Listener: "http", Host: "b.example", Path: "/"}})
	targets, err := New(rt, h, home).Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.Lookup(ContainerName); !ok || len(targets) != 2 {
		t.Fatalf("gateway missing, targets %+v", targets)
	}
	routes, err := New(rt, nil, home).Routes(ctx)
	if err != nil || len(routes) != 1 || routes[0] != (Exposed{Listener: "http:80", Route: "b.example/", Release: "b", Service: "web:80", Upstream: "10.213.0.11:80"}) {
		t.Fatalf("routes: %+v %v", routes, err)
	}
}

// testHost is a host whose runtime can run karga's proxies (or not), with fake proxies.
func testHost(supported bool) *proxyops.Host {
	h := &proxyops.Host{Proxies: &fakeProxies{}, Net: hostnet.Net{}}
	if supported {
		h.UseRuntime(ports.RuntimeInfo{Capabilities: []string{ports.CapHostGateway}})
	}
	return h
}

// fakeProxies writes real configuration files and reports every proxy ready and drained.
type fakeProxies struct{ fakeruntime.Proxies }

func (f *fakeProxies) Write(dir string, cfg ports.ProxyConfig) error {
	return envoy.Write(dir, cfg)
}

func (f *fakeProxies) WaitReady(context.Context, string, map[string]string, time.Duration) error {
	return nil
}

func (f *fakeProxies) WaitDrained(context.Context, string, []string, time.Duration) (bool, error) {
	return true, nil
}

// Expose checks again under the gateway lock: what another release took meanwhile is refused and
// nothing is written.
func TestExposeChecksUnderTheLock(t *testing.T) {
	h := testHost(true)
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	proxyOf(t, rt, home, "other", []Port{{Service: "web", Port: 80}}, []Route{
		{Service: "web", Port: 80, Listener: "http", Host: "a.example", Path: "/"},
	})
	svc := resource.Resource{Kind: resource.KindService, Metadata: resource.Metadata{Name: "s"}, Spec: &resource.ServiceSpec{
		Workload: "w", Ports: []resource.ServicePort{{Name: "p", Port: 80, TargetPort: "p"}},
		Expose: []resource.Exposure{{Port: "p", Listener: "http", Host: "a.example", Path: "/"}}}}
	_, err := New(rt, h, home).Expose(ctx, "mine", "", []resource.Resource{svc},
		[]Route{{Service: "s", Port: 80, Listener: "http", Host: "a.example", Path: "/"}})
	if err == nil || !strings.Contains(err.Error(), "already routed to other/web") {
		t.Fatalf("Expose: %v", err)
	}
	if _, routes, _ := readExpose(home, "mine"); routes != nil {
		t.Fatalf("a refused exposure was written: %+v", routes)
	}
}

// Waiting for the gateway's lock ends with the context.
func TestLockHonoursContext(t *testing.T) {
	home := t.TempDir()
	unlock, err := Lock(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, home); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the wait to end with ctx, got %v", err)
	}
}

// A gateway another KARGA_HOME's karga-server runs is left alone: replacing it would make the two
// servers replace each other's for ever.
func TestSyncRefusesAnotherHomesGateway(t *testing.T) {
	h := testHost(true)
	ctx := context.Background()
	rt := fakeruntime.New("img", envoy.Image())
	route := []Route{{Service: "web", Port: 80, Listener: "http", Host: "b.example", Path: "/"}}
	first, second := t.TempDir(), t.TempDir()
	proxyOf(t, rt, first, "b", []Port{{Service: "web", Port: 80}}, route)
	if _, err := New(rt, h, first).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	proxyOf(t, rt, second, "c", []Port{{Service: "web", Port: 80}}, route)
	rt.Calls = nil
	if _, err := New(rt, h, second).Sync(ctx); !errors.Is(err, ports.ErrConflict) || !strings.Contains(err.Error(), first) {
		t.Fatalf("want a conflict naming the other home, got %v", err)
	}
	if slices.Contains(rt.Calls, "DeleteWorkload:"+ContainerName) {
		t.Fatalf("the other home's gateway was removed: %v", rt.Calls)
	}
}

func TestListenerOptions(t *testing.T) {
	web := Listener{Name: "http", Port: 80, Protocol: ProtocolHTTP, ListenerOptions: ListenerOptions{
		Timeout: "30s", IdleTimeout: "10m", MaxBody: "1Mi", RateLimit: "60/m"}}
	pg := Listener{Name: "pg", Port: 5432, Protocol: ProtocolTCP, ListenerOptions: ListenerOptions{IdleTimeout: "1h", ProxyProtocol: "v2"}}
	settings := Settings{Listen: []string{"127.0.0.1"}, Listeners: []Listener{web, pg}}
	if err := validateSettings(settings); err != nil {
		t.Fatal(err)
	}
	targets := []Target{{Release: "a", IP: "10.213.0.2", Ports: []Port{{Service: "shop", Port: 80}, {Service: "db", Port: 5432}},
		Routes: []Route{{Service: "shop", Port: 80, Listener: "http", Host: "shop.example", Path: "/"}, {Service: "db", Port: 5432, Listener: "pg"}}}}
	cfg := buildConfig(settings, targets, "/etc/karga-proxy")
	want := ports.ProxyHTTPLimits{Timeout: 30 * time.Second, IdleTimeout: 10 * time.Minute, MaxBodyBytes: 1 << 20, RateRequests: 60, RatePer: time.Minute}
	if len(cfg.HTTP) != 1 || cfg.HTTP[0].Limits != want {
		t.Fatalf("http limits: %+v", cfg.HTTP)
	}
	// The PROXY header goes on a cluster of its own, to the same upstream.
	if len(cfg.TCP) != 1 || cfg.TCP[0].Cluster != "a_db_p5432_ppv2" || cfg.TCP[0].IdleTimeout != time.Hour {
		t.Fatalf("tcp: %+v", cfg.TCP)
	}
	i := slices.IndexFunc(cfg.Clusters, func(c ports.ProxyCluster) bool { return c.Name == "a_db_p5432_ppv2" })
	if i < 0 || cfg.Clusters[i].Host != "10.213.0.2" || cfg.Clusters[i].Port != 5432 || cfg.Clusters[i].ProxyProtocol != "v2" {
		t.Fatalf("clusters: %+v", cfg.Clusters)
	}
	// Readiness waits for the clusters the listeners use.
	if want := Want(settings, targets, func(Target) bool { return true }); len(want) != 2 || want["a_db_p5432_ppv2"] != "10.213.0.2:5432" || want["a_shop_p80"] != "10.213.0.2:80" {
		t.Fatalf("want: %v", want)
	}

	for opts, msg := range map[ListenerOptions]string{
		{Timeout: "soon"}:     `timeout: "soon" is not a duration`,
		{MaxBody: "17Mi"}:     "between 1 and 16Mi",
		{RateLimit: "100/h"}:  "requests per second or minute",
		{ProxyProtocol: "v2"}: "proxyProtocol cannot be set on a http listener",
		{IdleTimeout: "-1s"}:  "is not a duration",
	} {
		l := web
		l.ListenerOptions = opts
		if err := validateSettings(Settings{Listeners: []Listener{l}}); err == nil || !strings.Contains(err.Error(), msg) || !errors.Is(err, ports.ErrInvalid) {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	pg.Timeout, pg.RateLimit = "1s", "1/s"
	if err := validateSettings(Settings{Listeners: []Listener{pg}}); err == nil || !strings.Contains(err.Error(), "rateLimit, timeout cannot be set on a tcp listener") {
		t.Errorf("tcp: %v", err)
	}
}

// While a proxy replaces another both are labeled: the gateway routes to the one the expose file
// names, or else to the newer one.
func TestTargetsPickOneProxyPerRelease(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	for i, name := range []string{"a-karga-proxy-1111aaaa", "a-karga-proxy-2222bbbb"} {
		if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: name, Image: "img", Labels: map[string]string{ports.LabelManaged: "true",
			ports.LabelRelease: "a", ports.LabelRevision: strconv.Itoa(9 + i), LabelEdgeIP: "10.213.0." + strconv.Itoa(20+i)}}); err != nil {
			t.Fatal(err)
		}
	}
	routes := []Route{{Service: "web", Port: 80, Listener: "http", Host: "a.example", Path: "/"}}
	for proxy, want := range map[string]string{"a-karga-proxy-1111aaaa": "a-karga-proxy-1111aaaa", "": "a-karga-proxy-2222bbbb"} {
		if err := writeExpose(home, "a", proxy, routes); err != nil {
			t.Fatal(err)
		}
		targets, err := findTargets(ctx, rt, home)
		if err != nil || len(targets) != 1 || targets[0].Container != want {
			t.Fatalf("expose names %q: %+v %v", proxy, targets, err)
		}
	}
}

// A gateway made from another image or limits is handed over: the new one starts and routes
// before the old one is removed.
func TestSyncHandsOverToANewGateway(t *testing.T) {
	h := testHost(true)
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img", envoy.Image())
	oldSource := source(ports.ProxyContainer{Source: "old"}, Dir(home))
	old := containerName(oldSource)
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: old, Image: "img", Labels: map[string]string{
		ports.LabelManaged: "true", LabelComponent: "gateway", labelSource: oldSource}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(ctx, old); err != nil {
		t.Fatal(err)
	}
	proxyOf(t, rt, home, "b", []Port{{Service: "web", Port: 80}}, []Route{{Service: "web", Port: 80, Listener: "http", Host: "b.example", Path: "/"}})
	rt.Raw = nil
	if _, err := New(rt, h, home).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	gws, _ := gateways(ctx, rt)
	if len(gws) != 1 || gws[0].Name == old || !strings.HasPrefix(gws[0].Name, ContainerName+"-") {
		t.Fatalf("gateways after the hand-over: %+v", gws)
	}
	next := gws[0].Name
	if a, b := slices.Index(rt.Raw, "StartWorkload:"+next), slices.Index(rt.Raw, "DeleteWorkload:"+old); a < 0 || b < 0 || a > b {
		t.Fatalf("the new gateway must start before the old one goes: %v", rt.Raw)
	}
	if cmd := rt.Workloads[next].Request.Command; !slices.Contains(cmd, "/etc/karga-proxy/"+instance(next)+"/envoy.yaml") {
		t.Fatalf("the new gateway runs its own bootstrap: %v", cmd)
	}
	if _, err := os.Stat(filepath.Join(Dir(home), instance(next), "envoy.yaml")); err != nil {
		t.Fatal(err)
	}
	if name, err := New(rt, h, home).Current(ctx); err != nil || name != next {
		t.Fatalf("current: %s %v", name, err)
	}
}

func TestSettingsChanges(t *testing.T) {
	a := Settings{Listen: []string{"127.0.0.1"}, Listeners: []Listener{{Name: "web", Port: 80, Protocol: ProtocolHTTP}, {Name: "old", Port: 81, Protocol: ProtocolHTTP}}}
	b := Settings{Listen: []string{"0.0.0.0"}, AccessLog: true, Listeners: []Listener{
		{Name: "web", Port: 8080, Protocol: ProtocolHTTP, ListenerOptions: ListenerOptions{RateLimit: "10/s"}},
		{Name: "db", Port: 5432, Protocol: ProtocolTCP, ListenerOptions: ListenerOptions{ProxyProtocol: "v2"}}}}
	var got []string
	for _, c := range settingsChanges(a, b) {
		got = append(got, c.Action+" "+c.Kind+" "+c.Name+": "+strings.Join(c.Fields, ", "))
	}
	want := []string{"change listen : 127.0.0.1 -> 0.0.0.0", "change accessLog : off -> on",
		"change listener web: port: 80 -> 8080, rateLimit: - -> 10/s", "remove listener old: ",
		"add listener db: port: 5432, protocol: tcp, proxyProtocol: v2"}
	if !slices.Equal(got, want) {
		t.Fatalf("changes:\n%s", strings.Join(got, "\n"))
	}
}

func TestDocumentsAndRevisions(t *testing.T) {
	s, err := ParseDocument([]byte("apiVersion: karga.io/v1alpha1\nkind: Gateway\n"))
	if err != nil || !slices.Equal(s.Listen, []string{DefaultListen}) || !slices.Equal(s.Listeners, defaultListeners()) {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	for doc, want := range map[string]string{
		"kind: Gateway\n": `apiVersion ""`,
		"apiVersion: karga.io/v1alpha1\nkind: Gateway\nspec:\n  listenrs: []\n": "field listenrs not found",
		"apiVersion: karga.io/v1alpha1\nkind: Gateway\nextra: 1\n":              "field extra not found",
	} {
		if _, err := ParseDocument([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", doc, err)
		}
	}
	in := Settings{Listen: []string{"0.0.0.0"}, AccessLog: true, Listeners: []Listener{{Name: "pg", Port: 5432, Protocol: ProtocolTCP,
		ListenerOptions: ListenerOptions{ProxyProtocol: "v2"}}}}
	if out, err := ParseDocument(Document(in)); err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: %+v %v\n%s", out, err, Document(in))
	}
	if out, _ := ParseDocument(Document(Settings{Listen: []string{"127.0.0.1"}})); out.Listeners == nil || len(out.Listeners) != 0 {
		t.Fatalf("no listeners is a choice, not the defaults: %+v", out)
	}

	home := t.TempDir()
	for i := range 12 {
		in.Listeners[0].Port = 5000 + i
		if n, err := WriteRevision(home, in, "alice", "apply"); err != nil || n != i+1 {
			t.Fatalf("revision %d: %d %v", i+1, n, err)
		}
	}
	revs, _ := Revisions(home)
	if len(revs) != keepRevisions || revs[0].Revision != 3 || revs[9].Revision != 12 {
		t.Fatalf("kept: %+v", revs)
	}
	if _, err := os.Stat(revisionPath(home, 2)); !os.IsNotExist(err) {
		t.Fatalf("revision 2 not pruned: %v", err)
	}
	if err := DropRevision(home, 12); err != nil {
		t.Fatal(err)
	}
	if s, n, err := RevisionSettings(home, 0); err != nil || n != 11 || s.Listeners[0].Port != 5010 {
		t.Fatalf("in force after the drop: %d %+v %v", n, s, err)
	}
	if _, _, err := RevisionSettings(home, 2); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("a pruned revision: %v", err)
	}
	if n, err := WriteRevision(home, in, "", "x"); err != nil || n != 12 {
		t.Fatalf("after a drop the number is reused: %d %v", n, err)
	}
}

// An http listener redirects to HTTPS only when it says so, on any port; each host goes to the
// https listener serving it.
func TestRedirectHTTPS(t *testing.T) {
	tls := func(l, host string) Route {
		return Route{Service: "web", Port: 80, Listener: l, Host: host, Path: "/", TLSSecret: "c", CertDir: "/c"}
	}
	targets := []Target{{Release: "a", IP: "10.213.0.2", Ports: []Port{{Service: "web", Port: 80}},
		Routes: []Route{tls("https", "a.example"), tls("alt", "b.example"), {Service: "web", Port: 80, Listener: "plain", Host: "p.example", Path: "/"}}}}
	settings := Settings{Listen: []string{"127.0.0.1"}, Listeners: []Listener{
		{Name: "plain", Port: 8080, Protocol: ProtocolHTTP},
		{Name: "https", Port: 443, Protocol: ProtocolHTTPS},
		{Name: "alt", Port: 8443, Protocol: ProtocolHTTPS},
	}}
	redirects := func(s Settings) map[string]int {
		for _, l := range buildConfig(s, targets, "/etc/karga-proxy").HTTP {
			if l.Port == 8080 {
				return l.Redirects
			}
		}
		return nil
	}
	if r := redirects(settings); r != nil {
		t.Fatalf("no redirectHTTPS, no redirects: %v", r)
	}
	settings.Listeners[0].RedirectHTTPS = true
	if r := redirects(settings); !maps.Equal(r, map[string]int{"a.example": 443, "b.example": 8443}) {
		t.Fatalf("redirects: %v", r)
	}
	bad := Settings{Listeners: []Listener{{Name: "s", Port: 443, Protocol: ProtocolHTTPS, ListenerOptions: ListenerOptions{RedirectHTTPS: true}}}}
	if err := validateSettings(bad); err == nil || !strings.Contains(err.Error(), "redirectHTTPS cannot be set on a https listener") {
		t.Fatalf("redirectHTTPS on https: %v", err)
	}
}

func TestEdgeAddresses(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	const subnet = "10.213.0.0/29" // .2 to .6 usable
	if err := EnsureEdge(ctx, rt, subnet); err != nil {
		t.Fatal(err)
	}
	if err := EnsureEdge(ctx, rt, subnet); err != nil {
		t.Fatalf("again: %v", err)
	}
	if n := rt.Networks[EdgeNetwork]; !n.Internal || n.Subnet != subnet || n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		t.Fatalf("edge network: %+v", n)
	}
	ip := func(rel, avoid string) string {
		t.Helper()
		got, err := EdgeIP(ctx, rt, home, subnet, rel, avoid)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// A release keeps its pair; a replace takes the other address of it.
	if a, a2, b := ip("a", ""), ip("a", ""), ip("a", "10.213.0.2"); a != "10.213.0.2" || a2 != a || b != "10.213.0.3" {
		t.Fatalf("pair of a: %s %s %s", a, a2, b)
	}
	if b := ip("a", "10.213.0.3"); b != "10.213.0.2" {
		t.Fatalf("back to the first: %s", b)
	}
	// A proxy holding an address (made elsewhere) is skipped.
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: "x", Image: "img", Labels: map[string]string{ports.LabelManaged: "true", LabelEdgeIP: "10.213.0.4"}}); err != nil {
		t.Fatal(err)
	}
	if c := ip("c", ""); c != "10.213.0.5" {
		t.Fatalf("c: %s", c)
	}
	// c took .5 and .6; a release that goes leaves its pair, given again only after the counter
	// wraps (here at once: the subnet is tiny).
	if err := ReleaseEdge(ctx, home, "a"); err != nil {
		t.Fatal(err)
	}
	if d := ip("d", ""); d != "10.213.0.2" {
		t.Fatalf("d after the wrap: %s", d)
	}
	if _, err := EdgeIP(ctx, rt, home, subnet, "e", ""); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("a full subnet: %v", err)
	}
}

// A route's path and its to reach the gateway's configuration and its status.
func TestRoutePathAndTo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	rt := fakeruntime.New("img")
	proxyOf(t, rt, home, "a", nil, []Route{{Service: "api", Port: 80, Listener: "http", Host: "a.example", Path: "/api", PathType: "prefix", To: "/"}})
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		t.Fatal(err)
	}
	cfg := buildConfig(Settings{Listen: []string{"127.0.0.1"}, Listeners: defaultListeners()}, targets, "/etc/karga-proxy")
	if len(cfg.HTTP) != 1 || len(cfg.HTTP[0].Routes) != 1 || cfg.HTTP[0].Routes[0].Path != "/api" || cfg.HTTP[0].Routes[0].To != "/" {
		t.Fatalf("routes: %+v", cfg.HTTP)
	}
	routes, err := New(rt, nil, home).Routes(ctx)
	if err != nil || len(routes) != 1 || routes[0].Route != "a.example/api → /" {
		t.Fatalf("status routes: %+v %v", routes, err)
	}
}
