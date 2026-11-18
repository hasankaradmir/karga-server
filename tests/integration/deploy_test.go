//go:build integration

// Package integration deploys the example packages on a real Docker Engine (tech design §19.3).
package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func render(t *testing.T, example, release string, sets ...string) []resource.Resource {
	t.Helper()
	p, err := packages.Load("../testdata/" + example)
	if err != nil {
		t.Fatal(err)
	}
	m, err := packages.Render(p, packages.Options{ReleaseName: release, Values: packages.ValueSources{Sets: packages.Sets(sets...)}})
	if err != nil {
		t.Fatal(err)
	}
	var rs []resource.Resource
	for _, d := range m.Documents {
		rs = append(rs, d.Resource)
	}
	return rs
}

// deploy installs an example (plan against nothing applied) and always removes everything it
// created, volumes included.
func deploy(t *testing.T, rt ports.Runtime, example, release string, sets ...string) (deployment.ExecRequest, error) {
	t.Helper()
	rs := render(t, example, release, sets...)
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hostEnv.UseRuntime(info)
	plan, err := hostEnv.Diff(release, rs, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := deployment.ExecRequest{
		Home: sharedHome, Release: release, Revision: 1,
		Supported: []string{"docker"}, Resources: rs, Plan: plan, Timeout: 3 * time.Minute,
	}
	t.Cleanup(func() {
		if err := hostEnv.Delete(context.Background(), rt, req.Home, release, true); err != nil {
			t.Errorf("cleanup %s: %v", release, err)
		}
	})
	return req, hostEnv.Execute(context.Background(), rt, req)
}

func managed(t *testing.T, rt ports.Runtime, release string) []ports.Object {
	t.Helper()
	objs, err := rt.ListManaged(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func hasObject(objs []ports.Object, kind, name string) bool {
	return slices.ContainsFunc(objs, func(o ports.Object) bool { return o.Kind == kind && o.Name == name })
}

func TestRedis(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	ctx := context.Background()
	release := releaseName("it-redis")
	l, _ := listener(t, gateway.ProtocolTCP)
	req, err := deploy(t, rt, "redis", release, "service.listener="+l)
	if err != nil {
		t.Fatal(err)
	}

	st, err := rt.GetWorkload(ctx, release+"-redis")
	if err != nil || st.State != ports.StateRunning || st.Health != ports.HealthHealthy {
		t.Fatalf("redis status: %+v, %v", st, err)
	}
	pub, err := rt.PublishedPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pub {
		if p.Release == release && (p.HostIP != "127.0.0.1" || p.Workload != deployment.ProxyName(release)) {
			t.Fatalf("only the release proxy publishes, on 127.0.0.1 only: %+v", pub)
		}
	}

	// A second release on the same tcp listener fails in pre-flight, before creating anything.
	clash := releaseName("it-clash")
	if _, err := deploy(t, rt, "redis", clash, "service.listener="+l); !errors.Is(err, ports.ErrPortInUse) {
		t.Fatalf("port clash: want ErrPortInUse, got %v", err)
	}
	if objs := managed(t, rt, clash); len(objs) != 0 {
		t.Fatalf("clashing release created objects: %+v", objs)
	}

	// Delete keeps the data volume by default (retention: retain).
	if err := hostEnv.Delete(ctx, rt, req.Home, release, false); err != nil {
		t.Fatal(err)
	}
	objs := managed(t, rt, release)
	if !hasObject(objs, ports.ObjectVolume, release+"-redis-data") || hasObject(objs, ports.ObjectWorkload, release+"-redis") {
		t.Fatalf("after delete: %+v", objs)
	}
}

func TestPostgres(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	release := releaseName("it-pg")
	// Postgres reads POSTGRES_PASSWORD_FILE as the postgres user (uid 999): healthy proves the
	// bind-mounted secret is readable by a non-root container user.
	if _, err := deploy(t, rt, "postgres", release, "database.password=s3cret"); err != nil {
		t.Fatal(err)
	}
	st, err := rt.GetWorkload(context.Background(), release+"-postgres")
	if err != nil || st.Health != ports.HealthHealthy {
		t.Fatalf("postgres status: %+v, %v", st, err)
	}
}

func TestSecretReadableByNonRoot(t *testing.T) {
	t.Parallel()
	rt := newRuntime(t)
	ctx := context.Background()
	release := releaseName("it-secret")
	home := t.TempDir()
	secret := resource.Resource{Kind: resource.KindSecret, Metadata: resource.Metadata{Name: "creds"},
		Spec: &resource.SecretSpec{Values: map[string]string{"password": "s3cret"}}}
	paths, err := deployment.Materialize(home, release, 1, []resource.Resource{secret}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureImage(ctx, "busybox:1.37", "if-not-present"); err != nil {
		t.Fatal(err)
	}
	name := release + "-reader"
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, name) })
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{
		Name: name, Image: "busybox:1.37", User: "999:999", RestartPolicy: "never",
		Labels:  map[string]string{ports.LabelManaged: "true", ports.LabelRelease: release},
		Command: []string{"sh", "-c", "test \"$(cat /run/secrets/creds/password)\" = s3cret"},
		Mounts:  []ports.Mount{{Type: ports.MountBind, Source: paths[deployment.Key{Kind: resource.KindSecret, Name: "creds"}], Target: "/run/secrets/creds", ReadOnly: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(ctx, name); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err := rt.GetWorkload(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == ports.StateExited {
			if st.ExitCode != 0 {
				t.Fatalf("uid 999 could not read the secret (exit %d)", st.ExitCode)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("reader did not finish")
}

// TestMemoryLimitIsEnforced: a workload going over its memory limit is killed and reported so.
func TestMemoryLimitIsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt := newRuntime(t)
	name := releaseName("it-oom")
	if err := rt.EnsureImage(ctx, "busybox:1.37", "if-not-present"); err != nil {
		t.Fatal(err)
	}
	req := ports.WorkloadRequest{
		Name: name, Image: "busybox:1.37", RestartPolicy: "never", MemoryBytes: 32 << 20, PidsLimit: 64,
		Labels:  map[string]string{ports.LabelManaged: "true", ports.LabelRelease: name},
		Command: []string{"sh", "-c", `a=$(head -c 200000000 /dev/zero | tr '\0' x); echo ${#a}`},
	}
	if err := rt.CreateWorkload(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.DeleteWorkload(context.Background(), name) })
	if err := rt.StartWorkload(ctx, name); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := rt.GetWorkload(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == ports.StateExited {
			// Under load Docker sometimes reports the kill (137) without the OOM flag.
			if (!st.OOMKilled && st.ExitCode != 137) || st.Limits.MemorySwapBytes != 32<<20 {
				t.Fatalf("expected an OOM kill at 32Mi without swap: %+v", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %s after 60s", st.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestReadOnlyRootFilesystem: the image is read-only and /tmp stays writable.
func TestReadOnlyRootFilesystem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt := newRuntime(t)
	name := releaseName("it-ro")
	if err := rt.EnsureImage(ctx, "busybox:1.37", "if-not-present"); err != nil {
		t.Fatal(err)
	}
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{
		Name: name, Image: "busybox:1.37", RestartPolicy: "never", NoNewPrivileges: true,
		ReadOnlyRootfs: true, Tmpfs: map[string]string{"/tmp": "rw,size=1m"},
		Labels:  map[string]string{ports.LabelManaged: "true", ports.LabelRelease: name},
		Command: []string{"sh", "-c", "touch /x 2>/dev/null && echo root-writable; touch /tmp/x && echo tmp-writable"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.DeleteWorkload(context.Background(), name) })
	if err := rt.StartWorkload(ctx, name); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := rt.GetWorkload(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == ports.StateExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container did not finish")
		}
		time.Sleep(200 * time.Millisecond)
	}
	var out strings.Builder
	if err := rt.Logs(ctx, name, -1, false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "root-writable") || !strings.Contains(out.String(), "tmp-writable") {
		t.Fatalf("output: %q", out.String())
	}
}
