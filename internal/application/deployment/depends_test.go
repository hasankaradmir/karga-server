package deployment

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// dependentManifest is replaceManifest with workload app depending on Service redis.
func dependentManifest(port int) []resource.Resource {
	rs := withWorkload(replaceManifest(port), "app", 8080)
	for _, r := range rs {
		if r.Kind == resource.KindWorkload && r.Metadata.Name == "app" {
			r.Spec.(*resource.WorkloadSpec).DependsOn = []string{"redis"}
		}
	}
	return rs
}

func TestDependsOnStartsAfterTheService(t *testing.T) {
	r, _ := newProxyRequest(t)
	r.req.Resources = dependentManifest(r.port())
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	c := r.rt.Calls
	before(t, c, "StartWorkload:cache-redis", "CreateWorkload:cache-karga-proxy")
	before(t, c, "StartWorkload:cache-karga-proxy", "WaitReady:proxy:karga-redis-r1")
	before(t, c, "WaitReady:proxy:karga-redis-r1", "CreateWorkload:cache-app")
	if n := strings.Count(strings.Join(c, "\n"), "CreateWorkload:cache-karga-proxy"); n != 1 {
		t.Fatalf("proxy created %d times", n)
	}
}

func TestDependsOnFailedDependency(t *testing.T) {
	r, _ := newProxyRequest(t)
	r.req.Resources = dependentManifest(r.port())
	r.rt.Health = ports.HealthUnhealthy
	if err := r.run(); err == nil {
		t.Fatal("expected failure")
	}
	if slices.Contains(r.rt.Calls, "CreateWorkload:cache-app") {
		t.Fatalf("app started although redis never got ready: %v", r.rt.Calls)
	}
}

func TestDependsOnValidation(t *testing.T) {
	withProxySupport(t, true)
	rs := dependentManifest(6379)
	if err := resource.ValidateSet(rs); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for _, r := range rs {
		if w, ok := r.Spec.(*resource.WorkloadSpec); ok && r.Metadata.Name == "app" {
			w.DependsOn = []string{"nope"}
		}
	}
	if err := resource.ValidateSet(rs); err == nil || !strings.Contains(err.Error(), `dependsOn: Service "nope" is not defined`) {
		t.Fatalf("unknown service: %v", err)
	}
	// redis depends on app's service and app on redis's: a cycle.
	rs = dependentManifest(6379)
	rs[0].Spec.(*resource.WorkloadSpec).DependsOn = []string{"app"}
	if _, err := Order(rs); err == nil || !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("cycle: %v", err)
	}
	// dependsOn changes no container.
	applied := appliedWithProxy(t, dependentManifest(6379))
	v2 := dependentManifest(6379)
	v2[len(v2)-2].Spec.(*resource.WorkloadSpec).DependsOn = nil
	p, err := testEnv.Diff("cache", v2, applied, "", nil)
	if err != nil || p.HasChanges() {
		t.Fatalf("dependsOn only: %s %v", actions(p), errors.Unwrap(err))
	}
}
