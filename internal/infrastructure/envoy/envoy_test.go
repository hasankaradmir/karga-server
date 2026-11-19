package envoy

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

var sample = Config{
	TCP: []TCPListener{{Address: "127.0.0.1", Port: 5432, Cluster: "db_pg_p5432", IdleTimeout: time.Hour}},
	HTTP: []HTTPListener{{Address: "0.0.0.0", Port: 80, Routes: []HTTPRoute{
		{Host: "app.local", Path: "/", Cluster: "app_web_p3000"},
		{Host: "app.local", Path: "/api", Cluster: "app_api_p8080"},
		{Host: "b.local", Path: "/", Cluster: "b_web_p3000"},
	}, Redirects: []string{"secure.example"}, RedirectPort: 443}, {Address: "0.0.0.0", Port: 443, Routes: []HTTPRoute{
		{Host: "secure.example", Path: "/", Cluster: "app_web_p3000"},
	}, Certs: map[string]Cert{"secure.example": {Chain: "/etc/karga-proxy/certs/app_443_secure.example.crt", Key: "/etc/karga-proxy/certs/app_443_secure.example.key"}},
		Limits: HTTPLimits{Timeout: 30 * time.Second, IdleTimeout: 10 * time.Minute, MaxBodyBytes: 1 << 20, RateRequests: 100, RatePer: time.Second}}},
	Clusters: []Cluster{
		{Name: "db_pg_p5432", Host: "127.0.0.1", Port: 41001, ProxyProtocol: "v2"},
		{Name: "web-p8080", Host: "karga-web-r2", Port: 8080, DNS: true, HealthPath: "/healthz"},
	},
}

func TestConfigGolden(t *testing.T) {
	boot, err := bootstrap("web", 0o600, "3f9a2c1b")
	if err != nil {
		t.Fatal(err)
	}
	lds, err := Listeners(sample)
	if err != nil {
		t.Fatal(err)
	}
	cds, err := Clusters(sample)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]byte{"envoy.yaml": boot, "lds.yaml": lds, "cds.yaml": cds} {
		golden := filepath.Join("testdata", name)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, got, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (run with -update)", err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from golden:\n%s", name, got)
		}
	}
}

func TestWriteAndEndpoints(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proxy")
	if e, err := Endpoints(dir); err != nil || len(e) != 0 {
		t.Fatalf("no config: %v %v", e, err)
	}
	if err := Write(dir, sample); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", info.Mode(), err)
	}
	before, _ := os.Stat(filepath.Join(dir, listenersFile))
	next := sample
	next.Clusters = []Cluster{{Name: "db_pg_p5432", Host: "127.0.0.1", Port: 41002}}
	if err := Write(dir, next); err != nil {
		t.Fatal(err)
	}
	if e, err := Endpoints(dir); err != nil || len(e) != 1 || e["db_pg_p5432"] != "127.0.0.1:41002" {
		t.Fatalf("endpoints: %v %v", e, err)
	}
	if after, _ := os.Stat(filepath.Join(dir, listenersFile)); !os.SameFile(before, after) {
		t.Fatal("unchanged listeners were rewritten")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 { // lds.yaml and cds.yaml
		t.Fatalf("leftover temp files: %v", entries)
	}
}

// fakeAdmin serves canned admin responses on the unix socket of a proxy directory.
func fakeAdmin(t *testing.T, clusters, stats func() string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "envoy-admin-") // short path: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir = filepath.Join(dir, "i") // an instance's directory, as WriteInstance makes it
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, adminSocket))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clusters":
			_, _ = w.Write([]byte(clusters()))
		case "/stats":
			_, _ = w.Write([]byte(stats()))
		case "/ready":
			_, _ = w.Write([]byte("LIVE\n"))
		case "/server_info":
			_, _ = w.Write([]byte(`{"version":"eb0a95ba/1.36.13/Clean/RELEASE/BoringSSL","state":"LIVE"}`))
		case "/config_dump":
			_, _ = w.Write([]byte(strings.Replace(`{"configs":[{"active_state":{"listener":{"filter_chains":[{"filters":[{"typed_config":{"cluster":"p8080"}}]}]}}},
			  {"warming_state":{"listener":{"filter_chains":[{"filters":[{"typed_config":{"cluster":"p9000"}}]}]}}}ERROR]}`, "ERROR", rejectedListener.Load().(string), 1)))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return dir
}

// rejectedListener is added to the fake admin API's listener dump (",{...}" or "").
var rejectedListener atomic.Value

func init() { rejectedListener.Store("") }

func TestAdmin(t *testing.T) {
	healthy := `{"cluster_statuses":[{"name":"p8080","host_statuses":[{"hostname":"karga-web-r2","address":{"socket_address":{"address":"10.0.0.2","port_value":8080}},
	  "health_status":{"eds_health_status":"HEALTHY"},"stats":[{"name":"cx_active","value":"1"}]}]}]}`
	failing := `{"cluster_statuses":[{"name":"p8080","host_statuses":[{"hostname":"karga-web-r2",
	  "health_status":{"eds_health_status":"HEALTHY","failed_active_health_check":true},"stats":[{"name":"cx_active"}]}]}]}`
	clusters, total := healthy, "3"
	dir := fakeAdmin(t, func() string { return clusters }, func() string { return "cluster.p8080.upstream_cx_active: " + total })
	a := newAdmin(dir)
	ctx := context.Background()

	if ok, err := a.Ready(ctx, map[string]string{"p8080": "karga-web-r2"}); err != nil || !ok {
		t.Fatalf("ready: %v %v", ok, err)
	}
	if ok, _ := a.Ready(ctx, map[string]string{"p8080": "karga-web-r1"}); ok {
		t.Fatal("wrong backend reported ready")
	}
	if ok, _ := a.Ready(ctx, map[string]string{"p9000": "karga-web-r2"}); ok {
		t.Fatal("cluster without an active listener reported ready")
	}
	if ok, err := a.Ready(ctx, map[string]string{"p8080": "10.0.0.2:8080"}); err != nil || !ok {
		t.Fatalf("ready by address: %v %v", ok, err)
	}
	if n, err := a.OldConnections(ctx, []string{"p8080"}); err != nil || n != 2 {
		t.Fatalf("old connections: %d %v", n, err)
	}
	clusters = failing
	if ok, _ := a.Ready(ctx, map[string]string{"p8080": "karga-web-r2"}); ok {
		t.Fatal("host failing health checks reported ready")
	}
	if err := a.WaitReady(ctx, map[string]string{"p8080": "karga-web-r2"}, 300*time.Millisecond); err == nil {
		t.Fatal("WaitReady should time out")
	}
	clusters = healthy
	rejectedListener.Store(`,{"name":"http_10.255.255.1_80","error_state":{"details":"cannot bind '10.255.255.1:80': Cannot assign requested address"}}`)
	if w, err := a.notReady(ctx, map[string]string{"p8080": "karga-web-r2"}); err != nil || !strings.Contains(w, "cannot bind '10.255.255.1:80'") {
		t.Fatalf("a rejected listener must not be ready: %q %v", w, err)
	}
	rejectedListener.Store("")
	clusters, total = healthy, "1"
	if drained, err := a.WaitDrained(ctx, []string{"p8080"}, time.Second); err != nil || !drained {
		t.Fatalf("drained: %v %v", drained, err)
	}
	total = "5"
	if drained, _ := a.WaitDrained(ctx, []string{"p8080"}, 300*time.Millisecond); drained {
		t.Fatal("should not be drained")
	}
}

func TestDefaultImagePinned(t *testing.T) {
	t.Setenv("KARGA_PROXY_IMAGE", "")
	if !strings.Contains(Image(), "@sha256:") {
		t.Fatalf("default image is not pinned by digest: %s", Image())
	}
	t.Setenv("KARGA_PROXY_IMAGE", "envoy:test")
	if Image() != "envoy:test" {
		t.Fatalf("override ignored: %s", Image())
	}
}

func TestInvalidConfigRefusedBeforeWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proxy")
	bad := Config{Clusters: []Cluster{{Name: "", Host: "127.0.0.1", Port: 8080}}}
	err := Write(dir, bad)
	if err == nil || !strings.Contains(err.Error(), "envoy config") {
		t.Fatalf("want a validation error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, clustersFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config was written: %v", err)
	}
}

func TestWriteWaitsForClustersBeforeListeners(t *testing.T) {
	known := `{"cluster_statuses":[]}`
	dir := fakeAdmin(t, func() string { return known }, func() string { return "" })
	old := clusterWait
	clusterWait = 300 * time.Millisecond
	t.Cleanup(func() { clusterWait = old })

	start := time.Now()
	if err := Write(filepath.Dir(dir), sample); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("listeners written before Envoy knew the clusters")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), listenersFile)); err != nil {
		t.Fatalf("listeners must be written after the wait anyway: %v", err)
	}

	known = `{"cluster_statuses":[{"name":"db_pg_p5432"},{"name":"web-p8080"},{"name":"new"}]}`
	next := sample
	next.Clusters = append(slices.Clone(sample.Clusters), Cluster{Name: "new", Host: "127.0.0.1", Port: 1})
	next.TCP = append(slices.Clone(sample.TCP), TCPListener{Address: "127.0.0.1", Port: 1, Cluster: "new"})
	start = time.Now()
	if err := Write(filepath.Dir(dir), next); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("waited although Envoy knew every cluster")
	}
}

func TestAccessLogAndHealth(t *testing.T) {
	cfg := sample
	cfg.AccessLog = true
	lds, err := Listeners(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// One per filter: the tcp proxy, the plain HTTP listener and the TLS host's chain.
	if n := strings.Count(string(lds), "envoy.access_loggers.stdout"); n != 3 {
		t.Fatalf("access loggers: %d, want 3", n)
	}
	off, _ := Listeners(sample)
	if strings.Contains(string(off), "access_log") {
		t.Fatal("access log on by default")
	}

	dir := fakeAdmin(t, func() string { return `{"cluster_statuses":[]}` }, func() string { return "" })
	a := newAdmin(dir)
	if live, err := a.Live(context.Background()); err != nil || !live {
		t.Fatalf("live: %v %v", live, err)
	}
	if v, err := a.Version(context.Background()); err != nil || v != "1.36.13" {
		t.Fatalf("version: %q %v", v, err)
	}
}
