package releases

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// TestGatewayDocuments: a gateway document is diffed and applied as a revision; a failed apply
// drops it and the revision before keeps serving; commands write revisions too.
func TestGatewayDocuments(t *testing.T) {
	ctx := WithActor(context.Background(), "alice")
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
	p, spare := strconv.Itoa(port), strconv.Itoa(port+1)
	doc := func(spec string) []byte {
		return []byte("apiVersion: karga.io/v1alpha1\nkind: Gateway\nspec:\n" + spec)
	}
	listeners := func(web string) string {
		return "  listen: [127.0.0.1]\n  listeners:\n    - {name: http, port: " + p + ", protocol: http" + web + "}\n    - {name: spare, port: " + spare + ", protocol: http}\n"
	}
	if _, err := svc.Gateway.Apply(ctx, doc(listeners("")), "", true); err != nil { // the defaults' websecure goes
		t.Fatal(err)
	}
	if _, err := svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "site", Package: webPkg,
		Values: Values{Sets: packages.Sets("route=site.local", "routeListener=http")}}, nil); err != nil {
		t.Fatal(err)
	}
	current := func() gateway.Settings {
		t.Helper()
		s, err := gateway.ReadSettings(home)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Problems refuse a document: a typo, a wrong kind, a value that does not validate, a listener
	// in use removed.
	for content, want := range map[string]string{
		string(doc("  listenrs: []\n")):                                             "field listenrs not found",
		"apiVersion: karga.io/v1alpha1\nkind: Gate\n":                               `kind "Gate"`,
		string(doc(listeners(", timeout: soon"))):                                   `timeout: "soon" is not a duration`,
		string(doc("  listeners:\n    - {name: spare, port: 1, protocol: http}\n")): "listener http is used by site/web",
	} {
		plan, err := svc.Gateway.Diff(ctx, []byte(content))
		if err != nil || !strings.Contains(strings.Join(plan.Problems, "\n"), want) {
			t.Errorf("%q: want a problem %q, got %+v %v", content, want, plan.Problems, err)
		}
		if _, err := svc.Gateway.Apply(ctx, []byte(content), "", true); !errors.Is(err, ports.ErrInvalid) {
			t.Errorf("%q: apply: %v", content, err)
		}
	}

	// A change: diffed, then applied as revision 2 by the actor; the same document again is a no-op.
	plan, err := svc.Gateway.Diff(ctx, doc(listeners(", timeout: 30s")))
	if err != nil || len(plan.Problems) != 0 || len(plan.Changes) != 1 || plan.Changes[0].Fields[0] != "timeout: - -> 30s" || plan.Risk != "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if rev, err := svc.Gateway.Apply(ctx, doc(listeners(", timeout: 30s")), "", false); err != nil || rev != 2 {
		t.Fatalf("apply: %d %v", rev, err)
	}
	if rev, err := svc.Gateway.Apply(ctx, doc(listeners(", timeout: 30s")), "", false); err != nil || rev != 2 {
		t.Fatalf("apply without a change: %d %v", rev, err)
	}
	if l := current().Listeners[0]; l.Timeout != "30s" {
		t.Fatalf("in force: %+v", l)
	}

	// Envoy refuses what karga's checks pass: a problem, found in a container of its own.
	rt.ValidateExit, rt.ValidateOutput = 1, "[critical][main] error initializing configuration '/etc/karga-proxy/envoy.yaml': bad certificate\n"
	plan, err = svc.Gateway.Diff(ctx, doc(listeners(", timeout: 45s")))
	if err != nil || len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "envoy: ") || !strings.Contains(plan.Problems[0], "bad certificate") {
		t.Fatalf("envoy refusal: %+v %v", plan.Problems, err)
	}
	if _, ok := rt.Lookup("karga-gateway-validate"); ok {
		t.Fatal("the validation container is left")
	}
	rt.ValidateExit = 0

	// The gateway does not route with the new settings: revision 3 is dropped, 2 keeps serving.
	px.failures.Store(1)
	if _, err := svc.Gateway.Apply(ctx, doc(listeners(", timeout: 60s")), "", false); err == nil || !strings.Contains(err.Error(), "the gateway kept revision 2") {
		t.Fatalf("failed apply: %v", err)
	}
	if l := current().Listeners[0]; l.Timeout != "30s" {
		t.Fatalf("in force after a failed apply: %+v", l)
	}

	// A risk (another address, a listener removed) needs the confirmation.
	risky := doc("  listen: [127.0.0.1, 0.0.0.0]\n  listeners:\n    - {name: http, port: " + p + ", protocol: http}\n")
	plan, _ = svc.Gateway.Diff(ctx, risky)
	if !strings.Contains(plan.Risk, "reachable on 0.0.0.0") || !strings.Contains(plan.Risk, "removes listener spare") {
		t.Fatalf("risk: %q", plan.Risk)
	}
	if _, err := svc.Gateway.Apply(ctx, risky, "", false); !errors.Is(err, ports.ErrInvalid) || !strings.Contains(err.Error(), "confirmation needed") {
		t.Fatalf("unconfirmed risk: %v", err)
	}
	if _, err := svc.Gateway.Apply(ctx, risky, "", true); err != nil {
		t.Fatal(err)
	}

	// A command writes a revision too; the history says who and how; a revision exports as a
	// document that applies again (a rollback).
	if err := svc.Gateway.SetAccessLog(ctx, true); err != nil {
		t.Fatal(err)
	}
	revs, err := svc.Gateway.History()
	var got []string
	for _, r := range revs {
		got = append(got, strconv.Itoa(r.Revision)+" "+r.Actor+" "+r.Source)
	}
	if err != nil || strings.Join(got, ",") != "1 alice apply,2 alice apply,3 alice apply,4 alice set access-log on" {
		t.Fatalf("history: %v %v", got, err)
	}
	old, err := svc.Gateway.Export(2)
	if err != nil {
		t.Fatal(err)
	}
	if rev, err := svc.Gateway.Apply(ctx, old, "rollback 2", true); err != nil || rev != 5 {
		t.Fatalf("rollback: %d %v", rev, err)
	}
	if s := current(); len(s.Listeners) != 2 || s.Listeners[0].Timeout != "30s" || s.AccessLog || len(s.Listen) != 1 {
		t.Fatalf("after the rollback: %+v", s)
	}
	if h, _ := svc.Gateway.Health(ctx); h.Revision != 5 {
		t.Fatalf("health revision: %d", h.Revision)
	}
}

// Changes to the gateway settings made at once all land: each reads the settings under the
// gateway lock, so none writes a revision without another's change (SA-08).
func TestConcurrentGatewayChangesAllLand(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			errs[i] = e.svc.Gateway.AddListener(context.Background(), gateway.Listener{Name: fmt.Sprintf("tcp%d", i), Port: 7000 + i, Protocol: "tcp"})
		})
	}
	wg.Wait()
	settings, _, err := e.svc.Gateway.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("listener %d: %v", i, err)
		}
		if !slices.ContainsFunc(settings.Listeners, func(l gateway.Listener) bool { return l.Name == fmt.Sprintf("tcp%d", i) }) {
			t.Fatalf("listener tcp%d was lost: %+v", i, settings.Listeners)
		}
	}
}
