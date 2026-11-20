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

	"github.com/hasankaradmir/karga-sdk/packages"

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
		return releases.Values{Sets: packages.Sets(append([]string{startDelay, "listener=" + l, "second=true"}, extra...)...)}
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
		if p.Release == name {
			t.Fatalf("a release publishes no port, its proxy included (the gateway uses the edge network): %+v", p)
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
	for _, c := range []string{gatewayName(t, rt), proxyName(t, rt, name), recordedContainer(t, s, name, "web")} {
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
	t.Cleanup(func() { _ = hostEnv(sharedHome).Delete(context.Background(), rt, b, true) })
	l, port := listener(t, gateway.ProtocolHTTP)
	install := func(rel, host, content string) error {
		_, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: rel, Package: webPkg,
			Values: releases.Values{Sets: packages.Sets([]string{"route=" + host, "routeListener=" + l, "content=" + content}...)}}, nil)
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
	before, _ := rt.GetWorkload(ctx, gatewayName(t, rt))
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
		if err := s.Containers.Logs(ctx, releases.LogsRequest{Gateway: true, Tail: 20}, &logged); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(logged.String(), `"GET /logged HTTP/1.1"`) {
		t.Fatalf("request not in the gateway access log:\n%s", logged.String())
	}
	if after, _ := rt.GetWorkload(ctx, gatewayName(t, rt)); after.Labels[ports.LabelManaged] == "" || before.Name != after.Name {
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
	if name := gatewayName(t, rt); name != "" {
		t.Fatalf("gateway %s still exists after the last release went", name)
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
		Values: releases.Values{Sets: packages.Sets([]string{"strategy=recreate", "listener=" + l, "listenPort=80"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, body := hostGet(t, port, "x"); code != http.StatusOK || body != "v1" {
		t.Fatalf("through the gateway: %d %q", code, body)
	}
	if w := workload(t, rt, containerName(t, rt, name, "web")); len(w.IPs) == 0 {
		t.Fatalf("workload: %+v", w)
	}
	pub, err := rt.PublishedPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pub {
		if p.Workload == containerName(t, rt, name, "web") {
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
		Values: releases.Values{Sets: packages.Sets([]string{"listenPort=80"}...)}}, nil); err != nil {
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
		return releases.Values{Sets: packages.Sets(append([]string{"echo=true", "echoListener=" + l, "drain=60s"}, extra...)...)}
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
		eps, err := envoy.Endpoints(deployment.ProxyDir(sharedHome, name))
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

// TestNoHealthCheckConnections: a tcp backend behind the release proxy and the gateway gets only
// its clients' connections. Envoy's empty health-check connections made sshd and fail2ban lock out
// every client behind the proxy's address (D44).
func TestNoHealthCheckConnections(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-gw-nohc")
	s, rt := service(t, name)
	l, echo := listener(t, gateway.ProtocolTCP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets("echo=true", "echoListener="+l)}}, nil); err != nil {
		t.Fatal(err)
	}
	conns := func() string {
		t.Helper()
		var out strings.Builder
		cmd := []string{"sh", "-c", "cat /tmp/conns 2>/dev/null | wc -l"}
		if _, err := rt.Exec(ctx, containerName(t, rt, name, "web"), ports.ExecOptions{Cmd: cmd, Stdout: &out, Stderr: &out}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	time.Sleep(5 * time.Second) // Envoy's checks ran every 500ms
	if n := conns(); n != "0" {
		t.Fatalf("the backend got %s connections from no client", n)
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(echo), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := bufio.NewReader(conn).ReadString('\n'); err != nil || got != "hi\n" {
		t.Fatalf("echo through the gateway: %q %v", got, err)
	}
	_ = conn.Close()
	if n := conns(); n != "1" {
		t.Fatalf("the backend got %s connections for one client", n)
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
				Values: releases.Values{Sets: packages.Sets([]string{"route=" + x.name + ".local", "routeListener=" + l, "content=" + x.name}...)}}, nil)
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
		Values: releases.Values{Sets: packages.Sets([]string{"second=true", "route=boot.local", "routeListener=" + l}...)}}, nil); err != nil {
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

// TestProxyReplaceWithoutDowntime: a Service added (new aliases) needs a new release proxy
// container: it starts next to the old one, the gateway switches to it, and the old one drains. A
// port newly exposed changes nothing of the proxy (D42). Neither the gateway nor the release's
// network sees an outage.
func TestProxyReplaceWithoutDowntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-pxr")
	s, rt := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	tl, _ := listener(t, gateway.ProtocolTCP)
	common := opts()
	common.Timeout = 30 * time.Second
	sets := []string{"route=pxr.local", "routeListener=" + l}
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets(sets...)}}, nil); err != nil {
		t.Fatal(err)
	}
	host := startHostPingerFor(port, "pxr.local")
	net := startNetPinger(t, rt, name, "http://web:8080/")
	time.Sleep(time.Second)

	for i, more := range [][]string{{"second=true"}, {"second=true", "echo=true", "echoListener=" + tl}} {
		replace := i == 0
		before := proxyName(t, rt, name)
		sets = append(sets, more...)
		res, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg,
			Values: releases.Values{Sets: packages.Sets(sets...)}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range res.Plan {
			if c.Kind == deployment.KindProxy && (c.Strategy == "replace") != replace {
				t.Fatalf("%v: proxy %+v", more, c)
			}
		}
		if after := proxyName(t, rt, name); (after != before) != replace {
			t.Fatalf("%v: proxy %s -> %s, replace %v", more, before, after, replace)
		}
		time.Sleep(time.Second)
	}
	host.halt()
	got := net.result()
	t.Logf("gateway: %d ok, %d failed; network: %s", host.ok.Load(), host.failed.Load(), got)
	if f := host.failed.Load(); f > maxRaceFailures || host.ok.Load() == 0 {
		t.Fatalf("%d requests through the gateway failed while the proxy was replaced (max %d)", f, maxRaceFailures)
	}
	// In the network a client that resolved the old proxy just before it went loses its request
	// (the DNS race of any container change).
	if got.Failed > maxRaceFailures || got.OK == 0 {
		t.Fatalf("%d requests in the release's network failed while the proxy was replaced (max %d): %s", got.Failed, maxRaceFailures, got)
	}
}
