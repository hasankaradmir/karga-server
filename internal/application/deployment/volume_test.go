package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func TestWithDependents(t *testing.T) {
	wl := func(name string, deps ...string) resource.Resource {
		return resource.Resource{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: name}, Spec: &resource.WorkloadSpec{Image: "i", DependsOn: deps}}
	}
	svc := func(name, workload string) resource.Resource {
		return resource.Resource{Kind: resource.KindService, Metadata: resource.Metadata{Name: name}, Spec: &resource.ServiceSpec{Workload: workload}}
	}
	rs := []resource.Resource{
		wl("worker", "app"), wl("app", "db"), wl("db"), wl("web"), wl("cache"),
		svc("db", "db"), svc("app", "app"), svc("cache", "cache"),
	}
	got, err := WithDependents(rs, []string{"db"})
	if err != nil || !slices.Equal(got, []string{"db", "app", "worker"}) {
		t.Fatalf("dependents of db: %v %v", got, err)
	}
	if got, _ := WithDependents(rs, []string{"web"}); !slices.Equal(got, []string{"web"}) {
		t.Fatalf("nothing depends on web: %v", got)
	}
}

// RecreateVolume leaves an empty volume and the same containers, created and not started, with
// their anonymous volumes.
func TestRecreateVolume(t *testing.T) {
	ctx := context.Background()
	rt := fakeruntime.New("redis:7.4")
	rt.ImageVolumes = map[string][]string{"redis:7.4": {"/var/cache"}}
	spec := &resource.WorkloadSpec{Image: "redis:7.4", Mounts: []resource.Mount{{Volume: "data", MountPath: "/data"}}}
	vol := ports.VolumeRequest{Name: "shop-data", Driver: "local", Labels: labels("shop", 2, resource.KindVolume, "data")}
	if err := rt.CreateVolume(ctx, vol); err != nil {
		t.Fatal(err)
	}
	m := Mounter{Name: "redis", Container: "shop-redis-1a2b3c4d", Digest: "d1", Revision: 2, Spec: spec}
	lbl := labels("shop", 2, resource.KindWorkload, "redis")
	lbl[ports.LabelDigest] = "d1"
	wr, err := workloadRequest("shop", "redis", m.Container, spec, lbl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		t.Fatal(err)
	}
	before, _ := rt.GetWorkload(ctx, m.Container)
	write := func(name, content string) {
		t.Helper()
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
		_ = tw.Close()
		if err := rt.ImportPath(ctx, m.Container, "/data", &b); err != nil {
			t.Fatal(err)
		}
	}
	write("old.txt", "old")

	if err := testEnv.RecreateVolume(ctx, rt, "shop", vol, []Mounter{m}); err != nil {
		t.Fatal(err)
	}
	after, err := rt.GetWorkload(ctx, m.Container)
	if err != nil || after.State != ports.StateCreated || after.Labels[ports.LabelRevision] != "2" || after.Labels[ports.LabelDigest] != "d1" {
		t.Fatalf("container after: %+v %v", after, err)
	}
	if len(before.AnonymousVolumes) != 1 || len(after.AnonymousVolumes) != 1 || after.AnonymousVolumes[0].Source != before.AnonymousVolumes[0].Source {
		t.Fatalf("anonymous volumes: before %+v after %+v", before.AnonymousVolumes, after.AnonymousVolumes)
	}
	if len(rt.VolumeFiles["shop-data"]) != 0 {
		t.Fatalf("volume not empty: %v", rt.VolumeFiles["shop-data"])
	}
	if !maps.Equal(rt.Volumes["shop-data"].Labels, vol.Labels) {
		t.Fatalf("volume labels: %v", rt.Volumes["shop-data"].Labels)
	}
	write("new.txt", "new")
	if _, ok := rt.VolumeFiles["shop-data"]["new.txt"]; !ok {
		t.Fatal("the recreated container does not write into the new volume")
	}
	// The containers go before the volume (Docker refuses to remove a mounted one) and come back after.
	calls := strings.Join(rt.Calls, " ")
	if !strings.Contains(calls, "DeleteWorkload:shop-redis DeleteVolume:shop-data CreateVolume:shop-data CreateWorkload:shop-redis") {
		t.Fatalf("calls: %s", calls)
	}
}
