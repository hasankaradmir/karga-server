//go:build integration

package integration

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
)

// TestServicesAreIsolatedPerRelease: two releases both have a Service web on port 80; each resolves
// to its own workload on its own network.
func TestServicesAreIsolatedPerRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := releaseName("it-iso-a"), releaseName("it-iso-b")
	sa, rt := service(t, a)
	sb, _ := service(t, b)
	for _, x := range []struct {
		s    *releases.Service
		name string
	}{{sa, a}, {sb, b}} {
		if _, err := x.s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: x.name, Package: webPkg,
			Values: releases.Values{Sets: packages.Sets([]string{"servicePort=80", "content=" + x.name}...)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{a, b} {
		p := startNetPinger(t, rt, rel, "http://web:80/")
		time.Sleep(2 * time.Second)
		if res := p.result(); res.Bodies[rel] == 0 || len(res.Bodies) != 1 {
			t.Fatalf("%s: Service web should answer with its own content: %s", rel, res)
		}
	}
}

// TestGatewayListenAddresses: the gateway binds only the addresses the administrator sets. Not
// parallel: the listen addresses are host-wide.
func TestGatewayListenAddresses(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-listen")
	s, _ := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"route=listen.local", "routeListener=" + l}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	get := func(ip string) (int, error) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(ip, strconv.Itoa(port))+"/", nil)
		req.Host = "listen.local"
		// A fresh connection each time: Envoy lets connections on a removed listener finish.
		resp, err := (&http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := get("127.0.0.1"); err != nil || code != http.StatusOK {
		t.Fatalf("default 127.0.0.1: %d %v", code, err)
	}
	if err := s.Gateway.SetListen(ctx, []string{"127.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Gateway.SetListen(context.Background(), []string{gateway.DefaultListen}) })
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, err := get("127.0.0.2")
		_, oldErr := get("127.0.0.1")
		if err == nil && code == http.StatusOK && oldErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after set listen 127.0.0.2: new %d %v, old address error %v", code, err, oldErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// A listener in use cannot be removed.
	if err := s.Gateway.RemoveListener(ctx, l); err == nil {
		t.Fatal("removed a listener in use")
	}
}

// The gateway lets a request take longer than Envoy's default 15 seconds and passes WebSocket
// upgrades through.
func TestGatewayLongRequestsAndUpgrades(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-raw")
	s, _ := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"raw=true", "route=raw.local", "routeListener=" + l}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/x/slow", nil)
	req.Host = "raw.local"
	start := time.Now()
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("slow request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "slow" || time.Since(start) < 20*time.Second {
		t.Fatalf("slow request: %d %q after %s", resp.StatusCode, body, time.Since(start))
	}

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET /x/ws HTTP/1.1\r\nHost: raw.local\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	up, err := http.ReadResponse(r, nil)
	if err != nil || up.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", up, err)
	}
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := r.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo after upgrade: %q %v", line, err)
	}
}

// TestGatewayListenerLimits: the administrator's listener options reach the gateway without
// recreating it: a request timeout (504), a body limit (413), a rate limit (429) and, on a tcp
// listener, a PROXY protocol header the backend receives first.
func TestGatewayListenerLimits(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-limits")
	s, rt := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	tl, tport := listener(t, gateway.ProtocolTCP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"raw=true", "route=limits.local", "routeListener=" + l, "echo=true", "echoListener=" + tl}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	before, err := rt.GetWorkload(ctx, gatewayName(t, rt))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Gateway.SetListenerOptions(ctx, l, gateway.ListenerOptions{Timeout: "2s", MaxBody: "1Ki", RateLimit: "5/s"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Gateway.SetListenerOptions(ctx, tl, gateway.ListenerOptions{ProxyProtocol: "v1"}); err != nil {
		t.Fatal(err)
	}
	addr := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	do := func(method, path string, body io.Reader) int {
		t.Helper()
		req, _ := http.NewRequest(method, addr+path, body)
		req.Host = "limits.local"
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// Settings apply within moments (hot reload); the slow request is the sign they are in.
	deadline := time.Now().Add(15 * time.Second)
	for do(http.MethodGet, "/x/slow", nil) != http.StatusGatewayTimeout {
		if time.Now().After(deadline) {
			t.Fatal("timeout not applied")
		}
		time.Sleep(time.Second)
	}
	if code := do(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 2048))); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body: %d", code)
	}
	limited := 0
	for range 20 {
		if do(http.MethodGet, "/", nil) == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("20 requests at once passed a 5/s rate limit")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tport)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "hi\n"); err != nil {
		t.Fatal(err)
	}
	// The echo server sends back what it received: the PROXY header first.
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || !strings.HasPrefix(line, "PROXY TCP4 127.0.0.1 ") {
		t.Fatalf("proxy protocol: %q %v", line, err)
	}

	after, err := rt.GetWorkload(ctx, gatewayName(t, rt))
	if err != nil || after.Labels["io.karga.gateway-source"] != before.Labels["io.karga.gateway-source"] || after.RestartCount != before.RestartCount {
		t.Fatalf("the gateway was recreated or restarted: %+v %v", after, err)
	}
	t.Cleanup(func() {
		_ = s.Gateway.SetListenerOptions(context.Background(), l, gateway.ListenerOptions{})
		_ = s.Gateway.SetListenerOptions(context.Background(), tl, gateway.ListenerOptions{})
	})
}

// TestGatewayHandOffWithoutDowntime: a gateway made from another image (here the same image by
// digest alone) starts next to the old one on the same ports (SO_REUSEPORT); the old one closes its
// listeners and goes once the new one routes. Not parallel: the gateway is the host's.
func TestGatewayHandOffWithoutDowntime(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-handoff")
	s, rt := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"route=handoff.local", "routeListener=" + l}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	host := startHostPingerFor(port, "handoff.local")
	time.Sleep(time.Second)
	before := gatewayName(t, rt)
	repo, digest, _ := strings.Cut(envoy.DefaultImage, "@")
	repo, _, _ = strings.Cut(repo, ":")
	t.Setenv("KARGA_PROXY_IMAGE", repo+"@"+digest)
	orig, err := gateway.ReadSettings(sharedHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Gateway.SetAccessLog(ctx, !orig.AccessLog); err != nil { // a settings change syncs the gateway
		t.Fatal(err)
	}
	after := gatewayName(t, rt)
	time.Sleep(time.Second)
	host.halt()
	t.Logf("hand-over %s -> %s: %d ok, %d failed", before, after, host.ok.Load(), host.failed.Load())
	if after == before || after == "" {
		t.Fatalf("no new gateway: %s -> %s", before, after)
	}
	if f := host.failed.Load(); f > maxRaceFailures || host.ok.Load() == 0 {
		t.Fatalf("%d requests failed during the hand-over (max %d)", f, maxRaceFailures)
	}
	if h, err := s.Gateway.Health(ctx); err != nil || !h.Ready {
		t.Fatalf("health after the hand-over: %+v %v", h, err)
	}
	// Back to the default image for the other tests: another hand-over.
	_ = os.Unsetenv("KARGA_PROXY_IMAGE")
	if err := s.Gateway.SetAccessLog(ctx, orig.AccessLog); err != nil {
		t.Fatal(err)
	}
}

// TestGatewaySettingsFileApply: a gateway document applies as a revision; one the gateway cannot
// serve (an address the host does not have) is dropped and the revision before keeps serving; a
// rollback applies an earlier revision again. Not parallel: the settings are the host's.
func TestGatewaySettingsFileApply(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-gwfile")
	s, _ := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"raw=true", "route=gwfile.local", "routeListener=" + l}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	orig, err := gateway.ReadSettings(sharedHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = gateway.WriteSettings(sharedHome, orig)
		_ = s.Gateway.SetAccessLog(context.Background(), !orig.AccessLog) // syncs the gateway
		_ = s.Gateway.SetAccessLog(context.Background(), orig.AccessLog)
	})
	doc := func(change func(*gateway.Settings)) []byte {
		next := orig
		next.Listen = slices.Clone(orig.Listen)
		next.Listeners = slices.Clone(orig.Listeners)
		change(&next)
		return gateway.Document(next)
	}
	get := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+path, nil)
		req.Host = "gwfile.local"
		resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	before, err := s.Gateway.Export(0)
	if err != nil {
		t.Fatal(err)
	}
	timeout := doc(func(g *gateway.Settings) {
		i := slices.IndexFunc(g.Listeners, func(x gateway.Listener) bool { return x.Name == l })
		g.Listeners[i].Timeout = "2s"
	})
	if plan, err := s.Gateway.Diff(ctx, timeout); err != nil || len(plan.Problems) > 0 || len(plan.Changes) != 1 {
		t.Fatalf("diff: %+v %v", plan, err)
	}
	if _, err := s.Gateway.Apply(ctx, timeout, "", false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for get("/x/slow") != http.StatusGatewayTimeout {
		if time.Now().After(deadline) {
			t.Fatal("the applied timeout is not served")
		}
	}

	// The host has no 10.255.255.1: Envoy cannot bind it, the gateway keeps the revision it served.
	_, kept, _ := gateway.RevisionSettings(sharedHome, 0)
	if _, err := s.Gateway.Apply(ctx, doc(func(g *gateway.Settings) { g.Listen = append(g.Listen, "10.255.255.1") }), "", true); err == nil ||
		!strings.Contains(err.Error(), "the gateway kept revision "+strconv.Itoa(kept)) {
		t.Fatalf("apply of an address the host does not have: %v", err)
	}
	if code := get("/"); code != http.StatusOK {
		t.Fatalf("after the failed apply: %d", code)
	}

	// A rollback to the revision before the timeout: slow requests pass again.
	if _, err := s.Gateway.Apply(ctx, before, "rollback", true); err != nil {
		t.Fatal(err)
	}
	if code := get("/x/slow"); code != http.StatusOK {
		t.Fatalf("after the rollback: %d", code)
	}
}
