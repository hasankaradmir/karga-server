package deployment

import (
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// withProxySupport pretends to run on Linux (or not) for the duration of a test.
func withProxySupport(t *testing.T, on bool) {
	t.Helper()
	old := proxySupported.Load()
	proxySupported.Store(on)
	t.Cleanup(func() { proxySupported.Store(old) })
}

// replaceManifest is manifest() with a replaceable workload and Service redis on port.
func replaceManifest(port int) []resource.Resource {
	rs := manifest(port)
	w := rs[0].Spec.(*resource.WorkloadSpec)
	w.UpdateStrategy = &resource.UpdateStrategy{Type: resource.StrategyReplace, DrainTimeout: "2s"}
	w.Mounts[0].ReadOnly = true
	return append(rs, serviceRes("redis", "redis", port, "redis"))
}

// withWorkload adds a replaceable workload on network net listening on port, and a Service of the
// same name on that port.
func withWorkload(rs []resource.Resource, name string, port int) []resource.Resource {
	return append(rs, resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: name}, Spec: &resource.WorkloadSpec{
		Image: "app:1", PullPolicy: "if-not-present", RestartPolicy: "always", Networks: []string{"net"},
		Ports:          []resource.Port{{Name: "http", ContainerPort: port, Protocol: "tcp"}},
		HealthCheck:    &resource.HealthCheck{Type: "exec", Command: []string{"true"}},
		UpdateStrategy: &resource.UpdateStrategy{Type: resource.StrategyReplace},
	}}, serviceRes(name, name, port, "http"))
}

// serviceRes is a Service named name for workload with one port "p" going to target.
func serviceRes(name, workload string, port int, target string, expose ...resource.Exposure) resource.Resource {
	return resource.Resource{Kind: resource.KindService, Metadata: resource.Metadata{Name: name}, Spec: &resource.ServiceSpec{
		Workload: workload, Ports: []resource.ServicePort{{Name: "p", Port: port, TargetPort: target}}, Expose: expose}}
}

func changeOf(t *testing.T, p Plan, kind, name string) Change {
	t.Helper()
	for _, c := range p.Changes {
		if c.Kind == kind && (name == "" || c.Name == name) {
			return c
		}
	}
	return Change{}
}

// appliedWithProxy is the applied state after installing rs, proxy rows included.
func appliedWithProxy(t *testing.T, rs []resource.Resource) []release.AppliedResource {
	t.Helper()
	p, err := Diff("cache", rs, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []release.AppliedResource
	for _, c := range p.Changes {
		out = append(out, release.AppliedResource{Kind: c.Kind, Name: c.Name, RuntimeName: c.RuntimeName, Spec: c.Spec, Digest: c.Digest})
	}
	return out
}

func TestProxyPlan(t *testing.T) {
	withProxySupport(t, true)

	// Fresh install: one release proxy answering for the service.
	p, err := Diff("cache", replaceManifest(6379), nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	px := changeOf(t, p, KindProxy, "")
	if px.Action != ActionCreate || px.Name != ProxyResource || px.RuntimeName != "cache-karga-proxy" ||
		strings.Join(px.Proxy.Networks["net"], ",") != "redis" || px.Proxy.Published != nil {
		t.Fatalf("proxy create: %+v %+v", px, px.Proxy)
	}

	// A workload no service targeted gets its backend alias with a recreate.
	plain := appliedFrom(t, replaceManifest(6379)[:5])
	p, err = Diff("cache", replaceManifest(6379), plain, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Label() != "recreate: a service now targets it" || w.Fields[0].Path != "(service placement)" {
		t.Fatalf("service added: %+v", w)
	}

	// An image change follows the strategy; the proxy stays.
	applied := appliedWithProxy(t, replaceManifest(6379))
	v2 := replaceManifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	for override, want := range map[string]string{"": "replace", "recreate": "recreate"} {
		p, err = Diff("cache", v2, applied, override, nil)
		if err != nil {
			t.Fatal(err)
		}
		if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Label() != want {
			t.Fatalf("strategy %q: %+v", override, w)
		}
		if px := changeOf(t, p, KindProxy, ""); px.Action != ActionUnchanged {
			t.Fatalf("proxy should be unchanged: %+v", px)
		}
	}

	// A second service on the network reconnects the proxy with its alias.
	p, err = Diff("cache", withWorkload(replaceManifest(6379), "web", 8080), applied, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy, ""); px.Action != ActionUpdate || px.Label() != "reconnect: net: brief interruption" {
		t.Fatalf("add service: %+v", px)
	}

	// Exposing a port publishes it on the proxy container: a recreate.
	exposed := replaceManifest(6379)
	exposed[len(exposed)-1].Spec.(*resource.ServiceSpec).Expose = []resource.Exposure{{Port: "p", Listener: "pg"}}
	p, err = Diff("cache", exposed, applied, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy, ""); px.Strategy != proxyRecreate || px.Proxy.Published["redis"][0] != 6379 {
		t.Fatalf("expose: %+v %+v", px, px.Proxy)
	}

	// Without services the proxy goes and the workload loses its alias.
	p, err = Diff("cache", replaceManifest(6379)[:5], applied, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if px := changeOf(t, p, KindProxy, ""); px.Action != ActionDelete {
		t.Fatalf("remove proxy: %+v", px)
	}
	if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Label() != "recreate: no service targets it anymore" {
		t.Fatalf("remove service: %+v", w)
	}
}
