package deployment

import (
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/resource"
)

func res(kind, name string) resource.Resource {
	r := resource.Resource{Kind: kind, Metadata: resource.Metadata{Name: name}}
	switch kind {
	case resource.KindNetwork:
		r.Spec = &resource.NetworkSpec{}
	case resource.KindVolume:
		r.Spec = &resource.VolumeSpec{}
	}
	return r
}

func workload(name string, networks []string, mounts ...resource.Mount) resource.Resource {
	return resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: name},
		Spec: &resource.WorkloadSpec{Image: "x", Networks: networks, Mounts: mounts}}
}

func names(rs []resource.Resource) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Kind+"/"+r.Metadata.Name)
	}
	return strings.Join(s, ",")
}

func TestOrder(t *testing.T) {
	rs := []resource.Resource{
		workload("web", []string{"front"}, resource.Mount{Volume: "data", MountPath: "/d"}, resource.Mount{Secret: "creds", MountPath: "/s"}),
		workload("api", []string{"front"}),
		secretRes("creds", map[string]string{"p": "x"}),
		res(resource.KindVolume, "data"),
		res(resource.KindNetwork, "front"),
		cfgRes("unused", map[string]string{"a": "b"}),
	}
	got, err := Order(rs)
	if err != nil {
		t.Fatal(err)
	}
	want := "Network/front,Volume/data,Config/unused,Secret/creds,Workload/api,Workload/web"
	if names(got) != want {
		t.Fatalf("order:\n got %s\nwant %s", names(got), want)
	}
	// Input order must not matter.
	rev, err := Order(reverse(rs))
	if err != nil || names(rev) != want {
		t.Fatalf("not deterministic: %s %v", names(rev), err)
	}
	if names(reverse(got)) != "Workload/web,Workload/api,Secret/creds,Config/unused,Volume/data,Network/front" {
		t.Fatalf("reverse: %s", names(reverse(got)))
	}
}

func TestOrderErrors(t *testing.T) {
	if _, err := Order([]resource.Resource{workload("web", []string{"missing"})}); err == nil || !strings.Contains(err.Error(), `undefined Network "missing"`) {
		t.Fatalf("undefined dependency: %v", err)
	}
	a, b, c := res(resource.KindNetwork, "a"), res(resource.KindNetwork, "b"), res(resource.KindNetwork, "c")
	edges := map[string]Key{"a": {resource.KindNetwork, "b"}, "b": {resource.KindNetwork, "a"}}
	_, err := order([]resource.Resource{a, b, c}, func(r resource.Resource) []Key {
		if k, ok := edges[r.Metadata.Name]; ok {
			return []Key{k}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "circular dependency between Network/a, Network/b") {
		t.Fatalf("cycle: %v", err)
	}
}
