package releases

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"
	"github.com/hasankaradmir/karga-server/internal/ports"
	"github.com/hasankaradmir/karga-server/internal/ports/runtimetest"
)

// failingProxies is fakeruntime.Proxies whose next failures WaitReady calls fail.
type failingProxies struct {
	fakeruntime.Proxies
	failures atomic.Int32
}

func (f *failingProxies) WaitReady(context.Context, string, map[string]string, time.Duration) error {
	if f.failures.Add(-1) >= 0 {
		return errors.New("not ready")
	}
	return nil
}

// TestGatewaySettingsFile: gateway.yaml edited by hand is validated and applied by digest; a
// failed apply keeps the previous settings, and gateway commands wait until edits are applied.
func TestGatewaySettingsFile(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store, err := filestore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fakeruntime.New().WithGateway()
	px := &failingProxies{}
	host := &proxyops.Host{Proxies: px, Net: hostnet.Net{}}
	info, _ := rt.Info(ctx)
	host.UseRuntime(info)
	svc, err := New(Deps{Store: store, Runtime: rt, Host: host, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	port := runtimetest.FreePort(t)
	if err := gateway.WriteSettings(home, gateway.Settings{Listen: []string{"127.0.0.1"},
		Listeners: []gateway.Listener{{Name: "web", Port: port, Protocol: gateway.ProtocolHTTP}, {Name: "spare", Port: port + 1, Protocol: gateway.ProtocolHTTP}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "site", Package: webPkg,
		Values: Values{Sets: packages.Sets("route=site.local", "routeListener=web")}}, nil); err != nil {
		t.Fatal(err)
	}
	edit := func(yaml string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "gateway.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	applied := func() gateway.Settings {
		t.Helper()
		s, err := gateway.ReadSettings(home)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	p := strconv.Itoa(port)
	listeners := func(web string) string {
		return "listen: [127.0.0.1]\nlisteners:\n  - {name: web, port: " + p + ", protocol: http" + web + "}\n  - {name: spare, port: " + strconv.Itoa(port+1) + ", protocol: http}\n"
	}

	// Problems refuse the file: a typo, a value that does not validate, a listener in use removed.
	for yaml, want := range map[string]string{
		"listen: [127.0.0.1]\nlistenrs: []\n":                      "field listenrs not found",
		listeners(", timeout: soon"):                               `timeout: "soon" is not a duration`,
		"listeners:\n  - {name: spare, port: 1, protocol: http}\n": "listener web is used by site/web",
	} {
		edit(yaml)
		plan, err := svc.Gateway.ValidateSettings(ctx)
		if err != nil || len(plan.Problems) == 0 || !strings.Contains(strings.Join(plan.Problems, "\n"), want) {
			t.Errorf("%q: want a problem %q, got %+v %v", yaml, want, plan.Problems, err)
		}
	}

	// A valid edit: the plan lists it, commands wait for it, and it applies by digest.
	edit(listeners(", timeout: 30s"))
	plan, err := svc.Gateway.ValidateSettings(ctx)
	if err != nil || len(plan.Problems) != 0 || len(plan.Changes) != 1 || plan.Changes[0].Fields[0] != "timeout: - -> 30s" || plan.Risk != "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if h, err := svc.Gateway.Health(ctx); err != nil || !h.Pending {
		t.Fatalf("health before apply: %+v %v", h, err)
	}
	if err := svc.Gateway.SetAccessLog(ctx, true); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("a command with edits pending: %v", err)
	}
	if err := svc.Gateway.ApplySettings(ctx, "sha256:other", false); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("apply of another digest: %v", err)
	}
	if err := svc.Gateway.ApplySettings(ctx, plan.Digest, false); err != nil {
		t.Fatal(err)
	}
	if l := applied().Listeners[0]; l.Timeout != "30s" {
		t.Fatalf("applied: %+v", l)
	}
	if h, _ := svc.Gateway.Health(ctx); h.Pending {
		t.Fatal("pending after apply")
	}

	// The gateway does not route with the new settings: the previous ones stay.
	edit(listeners(", timeout: 60s"))
	plan, _ = svc.Gateway.ValidateSettings(ctx)
	px.failures.Store(1)
	if err := svc.Gateway.ApplySettings(ctx, plan.Digest, false); err == nil || !strings.Contains(err.Error(), "the gateway kept the previous settings") {
		t.Fatalf("failed apply: %v", err)
	}
	if l := applied().Listeners[0]; l.Timeout != "30s" {
		t.Fatalf("applied after a failed apply: %+v", l)
	}
	if data, _ := os.ReadFile(filepath.Join(home, "gateway.yaml")); !strings.Contains(string(data), "60s") {
		t.Fatalf("gateway.yaml must stay as written: %s", data)
	}

	// A risk (another address, a listener removed) needs the confirmation.
	edit("listen: [127.0.0.1, 0.0.0.0]\nlisteners:\n  - {name: web, port: " + p + ", protocol: http, timeout: 30s}\n")
	plan, _ = svc.Gateway.ValidateSettings(ctx)
	if !strings.Contains(plan.Risk, "reachable on 0.0.0.0") || !strings.Contains(plan.Risk, "removes listener spare") {
		t.Fatalf("risk: %q", plan.Risk)
	}
	if err := svc.Gateway.ApplySettings(ctx, plan.Digest, false); !errors.Is(err, ports.ErrInvalid) || !strings.Contains(err.Error(), "confirmation needed") {
		t.Fatalf("unconfirmed risk: %v", err)
	}
	if err := svc.Gateway.ApplySettings(ctx, plan.Digest, true); err != nil {
		t.Fatal(err)
	}
	if s := applied(); len(s.Listeners) != 1 || len(s.Listen) != 2 {
		t.Fatalf("applied: %+v", s)
	}
	// Applied: commands work again, and write both files.
	if err := svc.Gateway.SetAccessLog(ctx, true); err != nil {
		t.Fatal(err)
	}
	if h, _ := svc.Gateway.Health(ctx); h.Pending || !h.AccessLog {
		t.Fatalf("after a command: %+v", h)
	}
}
