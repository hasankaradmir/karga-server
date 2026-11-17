//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// TestKargaHealthChecks: tcp and http health checks need no tool in the image; karga runs them
// against the container's address.
func TestKargaHealthChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, health := range []string{"tcp", "http"} {
		t.Run(health, func(t *testing.T) {
			name := releaseName("it-hc-" + health)
			s, _ := service(t, name)
			sets := []string{"health=" + health, "strategy=recreate"}
			if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
				Values: packages.Options{Sets: sets}}); err != nil {
				t.Fatal(err)
			}
			rep, err := s.Status(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rep.Resources {
				if r.Kind == "Workload" && r.Health != ports.HealthHealthy {
					t.Fatalf("status health: %+v", r)
				}
			}
			if health != "http" {
				return
			}
			short := opts()
			short.Timeout = 20 * time.Second
			_, err = s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: short, Name: name, Package: webPkg,
				Values: packages.Options{Sets: append(sets, "healthy=false")}})
			if err == nil || !strings.Contains(err.Error(), "GET /missing") {
				t.Fatalf("want the failing http check in the error, got %v", err)
			}
		})
	}
}

// TestHealthCheckByContainerAddress: on Linux karga reaches an unpublished port at the container's
// network address.
func TestHealthCheckByContainerAddress(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-hc-ip")
	s, _ := service(t, name)
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: packages.Options{Sets: []string{"health=http"}}}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Status(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Resources {
		if r.Kind == "Workload" && r.Health != ports.HealthHealthy {
			t.Fatalf("status health: %+v", r)
		}
	}
}

// TestReplaceWithHTTPHealth: on Linux the proxies switch to the new container only once its HTTP
// health check passes; no request is lost.
func TestReplaceWithHTTPHealth(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-hc-replace")
	s, rt := service(t, name)
	l, port := listener(t, gateway.ProtocolTCP)
	common := opts()
	common.Timeout = 30 * time.Second
	sets := func(extra ...string) packages.Options {
		return packages.Options{Sets: append([]string{startDelay, "health=http", "listener=" + l}, extra...)}
	}
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets()}); err != nil {
		t.Fatal(err)
	}
	host := startHostPinger(port)
	defer host.halt()
	net := startNetPinger(t, rt, name, "http://web:8080/")
	time.Sleep(time.Second)
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg, Values: sets("content=v2")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	res := net.result()
	t.Logf("host: %d ok, %d failed; network: %s", host.ok.Load(), host.failed.Load(), res)
	if host.failed.Load() != 0 || res.Failed != 0 || host.seen()["v2"] == 0 {
		t.Fatalf("requests failed or v2 never served: host %v, network %s", host.seen(), res)
	}
}
