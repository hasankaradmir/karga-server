//go:build integration

package integration

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func TestGatewayReplaceWithoutLoss(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-gw")
	s, rt := service(t, name)
	l, port := listener(t, gateway.ProtocolTCP)
	common := opts()
	common.Timeout = 30 * time.Second
	sets := func(extra ...string) releases.Values {
		return releases.Values{Set: append([]string{startDelay, "listener=" + l, "second=true"}, extra...)}
	}

	res, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var proxyCreated bool
	for _, c := range res.Plan {
		proxyCreated = proxyCreated || (c.Kind == deployment.KindProxy && c.Action == "create")
	}
	if !proxyCreated {
		t.Fatalf("no release proxy in the plan: %+v", res.Plan)
	}
	pub, err := rt.PublishedPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pub {
		if p.Release == name && (p.HostIP != "127.0.0.1" || p.Workload != deployment.ProxyName(name)) {
			t.Fatalf("only the release proxy publishes, and only loopback upstreams: %+v", p)
		}
		if p.Port == port {
			t.Fatalf("the host port must be held by the gateway, not a container: %+v", p)
		}
	}
	routes, err := s.Gateway.Routes(ctx)
	if err != nil || !slices.ContainsFunc(routes, func(r gateway.Exposed) bool {
		return r.Release == name && r.Listener == l+":"+strconv.Itoa(port) && r.Route == "tcp" && r.Service == "web:8080"
	}) {
		t.Fatalf("gateway routes: %+v %v", routes, err)
	}

	host := startHostPinger(port)
	defer host.halt()
	net := startNetPinger(t, rt, name, "http://web:8080/", "http://api:8081/")
	time.Sleep(time.Second)

	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets("content=v2")}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	// A broken version of web (api unchanged) never takes traffic and changes nothing that runs.
	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets("content=v2", "healthy=false")}, nil); err == nil {
		t.Fatal("unhealthy replace should fail")
	}
	time.Sleep(time.Second)

	pings := net.results()
	web, api := pings[0], pings[1]
	bodies := host.seen()
	t.Logf("host: %d ok, %d failed, bodies %v; network web: %s; api: %s", host.ok.Load(), host.failed.Load(), bodies, web, api)
	if host.failed.Load() != 0 || web.Failed != 0 || api.Failed != 0 {
		t.Fatalf("requests failed behind the gateway and proxy: web %s, api %s", web, api)
	}
	if bodies["v2"] == 0 || len(bodies) != 2 || api.Bodies["v2"] == 0 {
		t.Fatalf("unexpected content: %v", bodies)
	}
	if r := currentRelease(t, s, name); r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed replace: %+v", r)
	}

	rep, err := s.Reads.Status(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	var backend string
	for _, r := range rep.Resources {
		if r.Kind == deployment.KindProxy {
			backend = r.Backend
		}
	}
	if backend != deployment.BackendAlias("api", 2)+", "+deployment.BackendAlias("web", 2) {
		t.Fatalf("proxy backends: %q", backend)
	}

	// Restarting the gateway, the proxy or the backend (which may change its IP) keeps routing.
	for _, c := range []string{gateway.ContainerName, deployment.ProxyName(name), name + "-web"} {
		if err := rt.StopWorkload(ctx, c, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := rt.StartWorkload(ctx, c); err != nil {
			t.Fatal(err)
		}
		host.reset()
		deadline := time.Now().Add(30 * time.Second)
		for host.ok.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if host.ok.Load() == 0 {
			t.Fatalf("no traffic after restarting %s", c)
		}
	}
}

// TestGatewayHTTPRoutes shares one host port between two releases by Host header.
func TestGatewayHTTPRoutes(t *testing.T) {
	requireLinux(t)
	ctx := context.Background()
	a, b := releaseName("it-gw-a"), releaseName("it-gw-b")
	s, rt := service(t, a)
	t.Cleanup(func() { _ = hostEnv.Delete(context.Background(), rt, s.Home, b, true) })
	l, port := listener(t, gateway.ProtocolHTTP)
	install := func(rel, host, content string) error {
		_, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: rel, Package: webPkg,
			Values: releases.Values{Set: []string{"route=" + host, "routeListener=" + l, "content=" + content}}}, nil)
		return err
	}
	if err := install(a, "a.local", "from-a"); err != nil {
		t.Fatal(err)
	}
	if err := install(b, "b.local", "from-b"); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"a.local": "from-a", "b.local:" + strconv.Itoa(port): "from-b"} {
		if code, body := hostGet(t, port, host); code != http.StatusOK || body != want {
			t.Errorf("Host %s: %d %q, want %q", host, code, body, want)
		}
	}
	if code, _ := hostGet(t, port, "other.local"); code != http.StatusNotFound {
		t.Errorf("unknown host: %d", code)
	}
	health, err := s.Gateway.Health(ctx)
	if err != nil || !health.Ready || health.Version != "1.36.13" || !strings.Contains(health.Image, "@sha256:") {
		t.Fatalf("gateway health: %+v %v", health, err)
	}
	// The access log is off by default; turning it on logs requests without recreating the gateway.
	before, _ := rt.GetWorkload(ctx, gateway.ContainerName)
	if err := s.Gateway.SetAccessLog(ctx, true); err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(logged.String(), `"GET /logged HTTP/1.1"`) && time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/logged", nil)
		req.Host = "a.local"
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
		logged.Reset()
		if err := s.Reads.Logs(ctx, releases.LogsRequest{Gateway: true, Tail: 20}, &logged); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(logged.String(), `"GET /logged HTTP/1.1"`) {
		t.Fatalf("request not in the gateway access log:\n%s", logged.String())
	}
	if after, _ := rt.GetWorkload(ctx, gateway.ContainerName); after.Labels[ports.LabelManaged] == "" || before.Name != after.Name {
		t.Fatalf("gateway changed: %+v", after)
	}
	// The same host and path in a third release is refused before anything is created.
	c := releaseName("it-gw-c")
	if err := install(c, "a.local", "from-c"); err == nil || !strings.Contains(err.Error(), "already routed") {
		t.Fatalf("duplicate route: %v", err)
	}
	// Removing the last release removes the gateway.
	for _, rel := range []string{a, b} {
		if _, err := s.Lifecycle.Uninstall(ctx, releases.UninstallRequest{Name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.GetWorkload(ctx, gateway.ContainerName); err == nil {
		t.Fatal("gateway still exists after the last release went")
	}
}

// TestGatewayRecreateWorkload: a recreate workload is served through its Service like any other,
// and its own container publishes nothing.
func TestGatewayRecreateWorkload(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-gw-rc")
	s, rt := service(t, name)
	l, port := listener(t, gateway.ProtocolTCP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{"strategy=recreate", "listener=" + l, "listenPort=80"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, body := hostGet(t, port, "x"); code != http.StatusOK || body != "v1" {
		t.Fatalf("through the gateway: %d %q", code, body)
	}
	if w := workload(t, rt, name+"-web"); len(w.IPs) == 0 {
		t.Fatalf("workload: %+v", w)
	}
	pub, err := rt.PublishedPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pub {
		if p.Workload == name+"-web" {
			t.Fatalf("the workload published a port: %+v", p)
		}
	}
}

// TestProxyLowPort: the non-root release proxy listens on containerPort 80.
func TestProxyLowPort(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-proxy-80")
	s, rt := service(t, name)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{"listenPort=80"}}}, nil); err != nil {
		t.Fatal(err)
	}
	p := startNetPinger(t, rt, name, "http://web:80/")
	time.Sleep(2 * time.Second)
	if res := p.result(); res.Bodies["v1"] == 0 {
		t.Fatalf("proxy did not serve containerPort 80: %s", res)
	}
}

// TestGatewayKeepsLongConnections: a connection open before a replace keeps working on the old
// container, and the replace finishes as soon as it closes instead of waiting the drain timeout.
func TestGatewayKeepsLongConnections(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-gw-long")
	s, _ := service(t, name)
	l, echo := listener(t, gateway.ProtocolTCP)
	common := opts()
	common.Timeout = 30 * time.Second
	sets := func(extra ...string) releases.Values {
		return releases.Values{Set: append([]string{"echo=true", "echoListener=" + l, "drain=60s"}, extra...)}
	}
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets()}, nil); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(echo), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	roundTrip := func(msg string) {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte(msg + "\n")); err != nil {
			t.Fatalf("write %s: %v", msg, err)
		}
		got, err := r.ReadString('\n')
		if err != nil || strings.TrimSpace(got) != msg {
			t.Fatalf("echo %s: %q %v", msg, got, err)
		}
	}
	roundTrip("before")
	// The gateway forwards to the release proxy; the proxy switches between containers.
	upstream := func() string {
		eps, err := envoy.Endpoints(deployment.ProxyDir(s.Home, name))
		if err != nil {
			t.Fatal(err)
		}
		return eps["web-p9000"]
	}
	initial := upstream()
	if initial == "" {
		t.Fatal("echo port not on the release proxy")
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets("content=v2")}, nil)
		done <- err
	}()
	// Wait until the gateway sends new connections to the new container, then let it settle and
	// use the old connection again.
	deadline := time.Now().Add(30 * time.Second)
	for upstream() == initial {
		if time.Now().After(deadline) {
			t.Fatal("the release proxy never switched to the new container")
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("replace finished while a connection was still open (err %v)", err)
	default:
	}
	roundTrip("during")
	closed := time.Now()
	_ = conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(closed); waited > 20*time.Second {
			t.Fatalf("replace waited %s after the last connection closed", waited)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("replace did not finish after the connection closed")
	}
}

// TestGatewayConcurrentInstalls: two karga processes (two services, one KARGA_HOME) install releases
// sharing a host port at the same time; the gateway ends up serving both.
func TestGatewayConcurrentInstalls(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	home := sharedHome
	a, b := releaseName("it-gw-ca"), releaseName("it-gw-cb")
	sa, _ := serviceIn(t, home, a)
	sb, _ := serviceIn(t, home, b)
	l, port := listener(t, gateway.ProtocolHTTP)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, x := range []struct {
		s    *releases.Service
		name string
	}{{sa, a}, {sb, b}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = x.s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: x.name, Package: webPkg,
				Values: releases.Values{Set: []string{"route=" + x.name + ".local", "routeListener=" + l, "content=" + x.name}}}, nil)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{a, b} {
		if code, body := hostGet(t, port, rel+".local"); code != http.StatusOK || body != rel {
			t.Errorf("%s: %d %q", rel, code, body)
		}
	}
}

// TestGatewaySurvivesDockerRestart restarts the Docker daemon (like a reboot). Opt-in: it needs
// passwordless sudo and disturbs every container on the host.
func TestGatewaySurvivesDockerRestart(t *testing.T) {
	requireLinux(t)
	if os.Getenv("KARGA_TEST_DAEMON_RESTART") != "1" {
		t.Skip("set KARGA_TEST_DAEMON_RESTART=1 to restart the Docker daemon")
	}
	ctx := context.Background()
	name := releaseName("it-gw-boot")
	s, _ := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{"second=true", "route=boot.local", "routeListener=" + l}}}, nil); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sudo", "-n", "systemctl", "restart", "docker").CombinedOutput(); err != nil {
		t.Fatalf("restart docker: %v\n%s", err, out)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		code, body, err := tryGet(port, "boot.local")
		if err == nil && code == http.StatusOK && body == "v1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no traffic after the daemon restart: %d %q %v", code, body, err)
		}
		time.Sleep(time.Second)
	}
	// The release proxy is back too: api answers on the network.
	rt := newRuntime(t)
	p := startNetPinger(t, rt, name, "http://api:8081/")
	time.Sleep(2 * time.Second)
	if res := p.result(); res.Bodies["v1"] == 0 {
		t.Fatalf("release proxy not serving after the restart: %s", res)
	}
}
