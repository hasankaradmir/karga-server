package deployment

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func webWithPort(port int) resource.Resource {
	return resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: "web"}, Spec: &resource.WorkloadSpec{
		Image: "x", Ports: []resource.Port{{Name: "http", ContainerPort: port, Protocol: "tcp"}},
		Resources:   &resource.Resources{Memory: "64Mi", CPU: "1"},
		HealthCheck: &resource.HealthCheck{Type: "exec", Command: []string{"true"}},
	}}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func preflight(t *testing.T, rt *fakeruntime.Runtime, release string, rs ...resource.Resource) error {
	t.Helper()
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return Preflight(context.Background(), rt, info, t.TempDir(), release, []string{"docker"}, rs, nil)
}

func TestPreflightOK(t *testing.T) {
	rs := []resource.Resource{res(resource.KindNetwork, "n"), webWithPort(freePort(t))}
	rs[0].Spec = &resource.NetworkSpec{Driver: "bridge"}
	if err := preflight(t, fakeruntime.New(), "web", rs...); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightRuntimeAndCapabilities(t *testing.T) {
	rt := fakeruntime.New()
	rt.Capabilities = []string{ports.CapNetworkDriver("bridge")}
	info, _ := rt.Info(context.Background())
	vol := resource.Resource{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "d"},
		Spec: &resource.VolumeSpec{Driver: "nfs", MountOptions: []string{"nocopy"}}}
	err := Preflight(context.Background(), rt, info, t.TempDir(), "web", []string{"podman"}, []resource.Resource{vol, webWithPort(80)}, nil)
	for _, want := range []string{`does not support runtime "docker"`, "volume driver nfs", "mountOptions", "memory limit", "cpu limit", "exec healthcheck"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestPreflightExposures(t *testing.T) {
	withProxySupport(t, true)
	svc := serviceRes("web", "web", 80, "http", resource.Exposure{Port: "p", Listener: "nope", Host: "a.example", Path: "/"})
	err := preflight(t, fakeruntime.New(), "web", webWithPort(80), svc)
	if err == nil || !strings.Contains(err.Error(), "no such gateway listener") {
		t.Fatalf("unknown listener: %v", err)
	}
	// A listener nobody serves yet is bound once to see that no host process holds it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	home := t.TempDir()
	if err := gateway.WriteSettings(home, gateway.Settings{Listeners: []gateway.Listener{{Name: "busy", Port: l.Addr().(*net.TCPAddr).Port, Protocol: gateway.ProtocolTCP}}}); err != nil {
		t.Fatal(err)
	}
	rt := fakeruntime.New()
	info, _ := rt.Info(context.Background())
	svc = serviceRes("web", "web", 80, "http", resource.Exposure{Port: "p", Listener: "busy"})
	err = Preflight(context.Background(), rt, info, home, "web", []string{"docker"}, []resource.Resource{webWithPort(80), svc}, nil)
	if !errors.Is(err, ports.ErrPortInUse) || !strings.Contains(err.Error(), "another process on this host") {
		t.Fatalf("host process: %v", err)
	}
}

func TestIPOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1", "127.0.0.1", true}, {"0.0.0.0", "127.0.0.1", true}, {"::", "10.0.0.1", true},
		{"", "127.0.0.1", true}, {"127.0.0.1", "10.0.0.1", false},
	} {
		if got := ipOverlap(c.a, c.b); got != c.want {
			t.Errorf("ipOverlap(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestPreflightCapabilities(t *testing.T) {
	rt := fakeruntime.New()
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	web := func(add ...string) resource.Resource {
		r := webWithPort(freePort(t))
		r.Spec.(*resource.WorkloadSpec).Security = &resource.Security{CapDrop: []string{"ALL"}, CapAdd: add}
		return r
	}
	check := func(r resource.Resource, allowed ...string) error {
		return Preflight(context.Background(), rt, info, t.TempDir(), "web", []string{"docker"}, []resource.Resource{r}, allowed)
	}
	if err := check(web("NET_BIND_SERVICE", "CAP_CHOWN")); err != nil {
		t.Fatalf("default capabilities must pass: %v", err)
	}
	for _, c := range []string{"SYS_ADMIN", "ALL", "cap_sys_ptrace"} {
		if err := check(web(c)); err == nil || !strings.Contains(err.Error(), "--allow-capabilities") {
			t.Errorf("%s: want refusal, got %v", c, err)
		}
	}
	if err := check(web("NET_ADMIN"), "CAP_NET_ADMIN"); err != nil {
		t.Fatalf("an allowed capability must pass: %v", err)
	}
	if err := check(web("SYS_ADMIN"), "ALL"); err != nil {
		t.Fatalf("ALL allows everything: %v", err)
	}
}
