package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	return Settings{Listen: []string{"127.0.0.1", "10.0.0.5"}, Listeners: append(DefaultListeners(),
		Listener{Name: "pg", Port: 5432, Protocol: ProtocolTCP}, Listener{Name: "unused", Port: 8080, Protocol: ProtocolHTTP})}
}

func TestGatewayConfigListenersAndRedirects(t *testing.T) {
	targets := []Target{
		{Release: "a", Ports: []Port{{Service: "shop", Port: 80, Upstream: 4001}, {Service: "db", Port: 5432, Upstream: 4002}},
			Routes: []Route{
				{Service: "shop", Port: 80, Listener: "websecure", Host: "secure.example", Path: "/", TLSSecret: "cert", CertDir: "/c"},
				{Service: "shop", Port: 80, Listener: "web", Host: "plain.example", Path: "/"},
				{Service: "db", Port: 5432, Listener: "pg"},
			}},
		{Release: "b", Ports: []Port{{Service: "web", Port: 80, Upstream: 4003}},
			Routes: []Route{
				{Service: "web", Port: 80, Listener: "websecure", Host: "both.example", Path: "/", TLSSecret: "cert", CertDir: "/d"},
				{Service: "web", Port: 80, Listener: "web", Host: "both.example", Path: "/"},
			}},
	}
	cfg := buildConfig(testSettings(), targets, "/etc/karga-proxy")
	if len(cfg.Clusters) != 3 || cfg.Clusters[0].Name != "a_shop_p80" || cfg.Clusters[0].Port != 4001 {
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
	if c := tls[0].Certs; len(c) != 2 || !strings.HasPrefix(c["secure.example"].Chain, "/etc/karga-proxy/certs/a_websecure_secure.example_") {
		t.Fatalf("certs: %+v", c)
	}
	// secure.example gets a redirect; both.example is served plainly on 80, so it does not.
	if r := plain[0].Redirects; !slices.Equal(r, []string{"secure.example"}) || plain[0].RedirectPort != 443 {
		t.Fatalf("redirects: %v %d", r, plain[0].RedirectPort)
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
	targets := []Target{{Release: "a", Routes: []Route{{Service: "web", Port: 80, Listener: "websecure", Host: "secure.example", Path: "/", TLSSecret: "cert", CertDir: src}}}}
	if err := syncCerts(dir, targets, true); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "certs", certName("a", "websecure", "secure.example", src)+".key")
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
	label, _ := json.Marshal(ps)
	if err := rt.CreateWorkload(context.Background(), ports.WorkloadRequest{Name: rel + "-karga-proxy", Image: "img", Labels: map[string]string{
		ports.LabelManaged: "true", ports.LabelRelease: rel, ports.LabelName: "proxy", LabelPorts: string(label)}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteExpose(home, rel, routes); err != nil {
		t.Fatal(err)
	}
}

func TestCheckExposures(t *testing.T) {
	h := testHost(true)
	ctx := context.Background()
	home := t.TempDir()
	if err := WriteSettings(home, Settings{Listeners: append(DefaultListeners(), Listener{Name: "pg", Port: 5432, Protocol: ProtocolTCP})}); err != nil {
		t.Fatal(err)
	}
	rt := fakeruntime.New("img")
	proxyOf(t, rt, home, "other", []Port{{Service: "web", Port: 80, Upstream: 4001}, {Service: "db", Port: 5432, Upstream: 4002}}, []Route{
		{Service: "web", Port: 80, Listener: "websecure", Host: "secure.example", Path: "/", TLSSecret: "cert"},
		{Service: "db", Port: 5432, Listener: "pg"},
	})
	check := func(e resource.Exposure) string {
		svc := resource.Resource{Kind: resource.KindService, Metadata: resource.Metadata{Name: "s"}, Spec: &resource.ServiceSpec{
			Workload: "w", Ports: []resource.ServicePort{{Name: "p", Port: 80, TargetPort: "p"}}, Expose: []resource.Exposure{e}}}
		_, err := Check(ctx, rt, h, home, "mine", []resource.Resource{svc}) // warnings: ports a test cannot bind
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
		{resource.Exposure{Port: "p", Listener: "web"}, "needs a host"},
		{resource.Exposure{Port: "p", Listener: "websecure", Host: "a.example", Path: "/"}, "needs tls.secret"},
		{resource.Exposure{Port: "p", Listener: "web", Host: "a.example", Path: "/", TLS: tls}, "plain http listener"},
		{resource.Exposure{Port: "p", Listener: "pg"}, "used by other/db"},
		{resource.Exposure{Port: "p", Listener: "websecure", Host: "secure.example", Path: "/", TLS: &resource.ExposureTLS{Secret: "cert"}}, "already routed to other/web"},
		{resource.Exposure{Port: "p", Listener: "websecure", Host: "secure.example", Path: "/api", TLS: tls}, "another certificate"},
		{resource.Exposure{Port: "p", Listener: "websecure", Host: "mine.example", Path: "/", TLS: tls}, ""},
	} {
		if got := check(tt.e); tt.want == "" && got != "" || !strings.Contains(got, tt.want) {
			t.Errorf("%+v: want %q, got %q", tt.e, tt.want, got)
		}
	}
}

func TestSettings(t *testing.T) {
	home := t.TempDir()
	s, err := ReadSettings(home)
	if err != nil || !slices.Equal(s.Listen, []string{"127.0.0.1"}) || !slices.Equal(s.Listeners, DefaultListeners()) {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s.Listeners = append(s.Listeners, Listener{Name: "alt", Port: 80, Protocol: ProtocolHTTP})
	if err := WriteSettings(home, s); err == nil || !strings.Contains(err.Error(), "port 80 is used by listener \"web\"") {
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
	if _, err := Sync(ctx, rt, h, home); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.GetWorkload(ctx, ContainerName); err == nil {
		t.Fatal("gateway started with nothing exposed")
	}
	proxyOf(t, rt, home, "b", []Port{{Service: "web", Port: 80, Upstream: 4001}}, []Route{{Service: "web", Port: 80, Listener: "web", Host: "b.example", Path: "/"}})
	targets, err := Sync(ctx, rt, h, home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.GetWorkload(ctx, ContainerName); err != nil || len(targets) != 2 {
		t.Fatalf("gateway: %v, targets %+v", err, targets)
	}
	routes, err := Routes(ctx, rt, home)
	if err != nil || len(routes) != 1 || routes[0] != (Exposed{Listener: "web:80", Route: "b.example/", Release: "b", Service: "web:80", Upstream: "127.0.0.1:4001"}) {
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

func (f *fakeProxies) Write(dir, node string, mode int, cfg ports.ProxyConfig) error {
	return envoy.Write(dir, node, mode, cfg)
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
	proxyOf(t, rt, home, "other", []Port{{Service: "web", Port: 80, Upstream: 4001}}, []Route{
		{Service: "web", Port: 80, Listener: "web", Host: "a.example", Path: "/"},
	})
	svc := resource.Resource{Kind: resource.KindService, Metadata: resource.Metadata{Name: "s"}, Spec: &resource.ServiceSpec{
		Workload: "w", Ports: []resource.ServicePort{{Name: "p", Port: 80, TargetPort: "p"}},
		Expose: []resource.Exposure{{Port: "p", Listener: "web", Host: "a.example", Path: "/"}}}}
	_, err := Expose(ctx, rt, h, home, "mine", []resource.Resource{svc},
		[]Route{{Service: "s", Port: 80, Listener: "web", Host: "a.example", Path: "/"}})
	if err == nil || !strings.Contains(err.Error(), "already routed to other/web") {
		t.Fatalf("Expose: %v", err)
	}
	if routes, _ := ReadExpose(home, "mine"); routes != nil {
		t.Fatalf("a refused exposure was written: %+v", routes)
	}
}
