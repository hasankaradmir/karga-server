package deployment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func manifest(port int) []resource.Resource {
	return []resource.Resource{
		{Kind: resource.KindWorkload, Metadata: resource.Metadata{Name: "redis"}, Spec: &resource.WorkloadSpec{
			Image: "redis:7.4", PullPolicy: "if-not-present", RestartPolicy: "always",
			Command:  []string{"redis-server"},
			Networks: []string{"net"},
			Ports:    []resource.Port{{Name: "redis", ContainerPort: port, Protocol: "tcp"}},
			Mounts: []resource.Mount{
				{Volume: "data", MountPath: "/data"},
				{Config: "conf", MountPath: "/etc/redis"},
			},
			HealthCheck: &resource.HealthCheck{Type: "exec", Command: []string{"redis-cli", "ping"}, Interval: "10s", Timeout: "3s", Retries: 3, StartPeriod: "5s"},
			Resources:   &resource.Resources{Memory: "512Mi", CPU: "500m"},
		}},
		cfgRes("conf", map[string]string{"redis.conf": "x"}),
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "data"}, Spec: &resource.VolumeSpec{Driver: "local", Retention: "retain"}},
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "cache"}, Spec: &resource.VolumeSpec{Driver: "local", Retention: "delete"}},
		{Kind: resource.KindNetwork, Metadata: resource.Metadata{Name: "net"}, Spec: &resource.NetworkSpec{Driver: "bridge"}},
	}
}

// tracker records applied state the way the release service does.
type tracker struct {
	applied map[Key]release.AppliedResource
	steps   []release.Step
}

func (tr *tracker) list() []release.AppliedResource {
	var out []release.AppliedResource
	for _, a := range tr.applied {
		out = append(out, a)
	}
	return out
}

func (tr *tracker) onStep(rev int) func(Change, release.Step) error {
	return func(c Change, st release.Step) error {
		tr.steps = append(tr.steps, st)
		k := Key{c.Kind, c.Name}
		rn := c.RuntimeName
		if rn == "" {
			rn = "cache-" + c.Name
		}
		switch {
		case st.Status == StepKept:
		case c.Action == ActionDelete && st.Status == StepDone:
			delete(tr.applied, k)
		case st.Status == StepDone:
			tr.applied[k] = release.AppliedResource{Kind: c.Kind, Name: c.Name, Revision: rev, RuntimeName: rn, Spec: c.Spec, Digest: c.Digest}
		default:
			a := tr.applied[k]
			a.Kind, a.Name, a.RuntimeName, a.Spec, a.Digest = c.Kind, c.Name, rn, c.Spec, ""
			tr.applied[k] = a
		}
		return nil
	}
}

type request struct {
	strategy string
	t        *testing.T
	rt       *fakeruntime.Runtime
	tr       *tracker
	req      ExecRequest
}

func newRequest(t *testing.T, port int) *request {
	return &request{t: t, rt: fakeruntime.New(), tr: &tracker{applied: map[Key]release.AppliedResource{}},
		req: ExecRequest{Home: t.TempDir(), Release: "cache", Revision: 1, Supported: []string{"docker"}, Resources: manifest(port), Timeout: time.Second}}
}

// run plans r.req.Resources against the tracked state and executes it.
func (r *request) run() error {
	r.t.Helper()
	plan, err := testEnv.Diff("cache", r.req.Resources, r.tr.list(), r.strategy, nil)
	if err != nil {
		return err
	}
	r.req.Plan = plan
	r.req.OnStep = r.tr.onStep(r.req.Revision)
	return testEnv.Execute(context.Background(), r.rt, r.req)
}

func TestExecuteInstall(t *testing.T) {
	r := newRequest(t, freePort(t))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	if len(r.tr.applied) != 5 {
		t.Fatalf("applied: %v", r.tr.applied)
	}

	// Image pulled before anything is created; dependencies before the workload.
	idx := func(call string) int { return slices.Index(rt.Calls, call) }
	sequence := []string{"Pull:redis:7.4", "CreateNetwork:cache-net", "CreateVolume:cache-data", "CreateWorkload:cache-redis", "StartWorkload:cache-redis"}
	for i := 1; i < len(sequence); i++ {
		if idx(sequence[i-1]) < 0 || idx(sequence[i-1]) > idx(sequence[i]) {
			t.Fatalf("%s must come before %s: %v", sequence[i-1], sequence[i], rt.Calls)
		}
	}

	w := rt.Workloads["cache-redis"].Request
	if w.Labels[ports.LabelRelease] != "cache" || w.Labels[ports.LabelName] != "redis" || w.Labels[ports.LabelRevision] != "1" {
		t.Fatalf("labels: %v", w.Labels)
	}
	// No Service targets it: no alias and nothing published.
	if !slices.Equal(w.Networks, []string{"cache-net"}) || len(w.Aliases) != 0 || len(w.Ports) != 0 {
		t.Fatalf("networks/aliases/ports: %v %v %v", w.Networks, w.Aliases, w.Ports)
	}
	if w.MemoryBytes != 512<<20 || w.MilliCPU != 500 {
		t.Fatalf("ports/resources: %+v", w)
	}
	if w.HealthCheck.Interval != 10*time.Second || w.HealthCheck.Retries != 3 || w.HealthCheck.StartPeriod != 5*time.Second {
		t.Fatalf("healthcheck: %+v", w.HealthCheck)
	}
	if w.Mounts[0] != (ports.Mount{Type: ports.MountVolume, Source: "cache-data", Target: "/data"}) {
		t.Fatalf("volume mount: %+v", w.Mounts[0])
	}
	if m := w.Mounts[1]; m.Type != ports.MountBind || !m.ReadOnly || !strings.HasSuffix(m.Source, "releases/cache/1/configs/conf") {
		t.Fatalf("config mount: %+v", m)
	}
	if rt.Volumes["cache-data"].Labels[ports.LabelRetention] != "retain" {
		t.Fatalf("volume labels: %v", rt.Volumes["cache-data"].Labels)
	}
}

func TestExecuteImageFailureCreatesNothing(t *testing.T) {
	r := newRequest(t, freePort(t))
	rt := r.rt
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "missing/redis:0"
	err := r.run()
	if !errors.Is(err, ports.ErrImageNotFound) {
		t.Fatalf("want ErrImageNotFound, got %v", err)
	}
	if len(rt.Networks)+len(rt.Volumes)+len(rt.Workloads) != 0 {
		t.Fatalf("objects created despite image failure: %v", rt.Calls)
	}
}

func TestExecuteFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeruntime.Runtime)
		want  string
	}{
		{"unhealthy", func(r *fakeruntime.Runtime) { r.Health = ports.HealthUnhealthy }, "unhealthy"},
		{"never healthy", func(r *fakeruntime.Runtime) { r.Health = ports.HealthStarting }, "not ready after"},
		{"start fails", func(r *fakeruntime.Runtime) { r.Fail["StartWorkload:cache-redis"] = errors.New("boom") }, `Workload "redis": boom`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRequest(t, freePort(t))
			tt.setup(r.rt)
			r.req.Timeout = 50 * time.Millisecond
			err := r.run()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
		})
	}
}

func TestExecuteUpgradeRecreatesOnlyChangedWorkload(t *testing.T) {
	r := newRequest(t, freePort(t))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.rt.Calls = nil
	r.req.Revision = 2
	r.req.Resources = manifest(r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].ContainerPort)
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	want := []string{"StopWorkload:cache-redis", "DeleteWorkload:cache-redis", "CreateWorkload:cache-redis", "StartWorkload:cache-redis"}
	var got []string
	for _, c := range r.rt.Calls {
		if !strings.HasPrefix(c, "Info") && !strings.HasPrefix(c, "EnsureImage") && !strings.HasPrefix(c, "Pull") {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("upgrade calls: %v", got)
	}
	if r.rt.Workloads["cache-redis"].Request.Labels[ports.LabelRevision] != "2" || r.rt.Workloads["cache-redis"].Request.Image != "redis:7.2" {
		t.Fatalf("workload not recreated: %+v", r.rt.Workloads["cache-redis"].Request)
	}

	// Nothing changed: nothing touched.
	r.rt.Calls = nil
	r.req.Revision = 3
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.rt.Calls {
		if strings.Contains(c, "Workload") || strings.HasPrefix(c, "Create") {
			t.Fatalf("no-op upgrade touched the runtime: %v", r.rt.Calls)
		}
	}
}

func TestExecuteFailedUpgradeStopsButKeepsContainer(t *testing.T) {
	r := newRequest(t, freePort(t))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.req.Revision = 2
	r.req.Timeout = 50 * time.Millisecond
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	r.rt.Health = ports.HealthUnhealthy
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("want unhealthy, got %v", err)
	}
	w, ok := r.rt.Workloads["cache-redis"]
	if !ok || w.State != ports.StateExited {
		t.Fatalf("failed workload should be stopped and kept: %+v", w)
	}
	if r.tr.applied[Key{resource.KindWorkload, "redis"}].Digest != "" {
		t.Fatal("failed workload must be marked for recreation")
	}

	// "Rollback": the original spec recreates it even though the image matches nothing new.
	r.rt.Health = ""
	r.req.Revision = 3
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.4"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if w := r.rt.Workloads["cache-redis"]; w.State != ports.StateRunning || w.Request.Image != "redis:7.4" {
		t.Fatalf("rollback: %+v", w)
	}
}

func TestExecuteRemovesDeletedResources(t *testing.T) {
	r := newRequest(t, freePort(t))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.req.Revision = 2
	r.req.Resources = r.req.Resources[1:] // drop the workload
	r.req.Resources = slices.DeleteFunc(r.req.Resources, func(x resource.Resource) bool { return x.Metadata.Name == "cache" })
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.rt.Workloads["cache-redis"]; ok {
		t.Fatal("removed workload still exists")
	}
	if _, ok := r.rt.Volumes["cache-cache"]; ok {
		t.Fatal("removed volume with retention delete still exists")
	}
}

func TestExecuteRefusesForeignNetwork(t *testing.T) {
	r := newRequest(t, freePort(t))
	if err := r.rt.CreateNetwork(context.Background(), ports.NetworkRequest{Name: "cache-net"}); err != nil {
		t.Fatal(err)
	}
	if err := r.run(); err == nil || !strings.Contains(err.Error(), "not managed by this release") {
		t.Fatalf("want ownership error, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	for _, deleteVolumes := range []bool{false, true} {
		r := newRequest(t, freePort(t))
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		rt := r.rt
		if err := testEnv.Delete(ctx, rt, r.req.Home, r.req.Release, deleteVolumes); err != nil {
			t.Fatal(err)
		}
		if len(rt.Workloads) != 0 || len(rt.Networks) != 0 {
			t.Fatalf("workloads/networks left: %v %v", rt.Workloads, rt.Networks)
		}
		_, dataKept := rt.Volumes["cache-data"]
		_, cacheKept := rt.Volumes["cache-cache"]
		if dataKept == deleteVolumes || cacheKept {
			t.Fatalf("deleteVolumes=%v: data kept=%v, cache (retention delete) kept=%v", deleteVolumes, dataKept, cacheKept)
		}
		if !slices.Contains(rt.Calls, "StopWorkload:cache-redis") {
			t.Fatal("workload not stopped gracefully before delete")
		}
	}
}

// replaceable makes the test manifest's workload eligible for replace.
func replaceable(rs []resource.Resource) []resource.Resource {
	w := rs[0].Spec.(*resource.WorkloadSpec)
	w.UpdateStrategy = &resource.UpdateStrategy{Type: resource.StrategyReplace, DrainTimeout: "3s"}
	w.Ports = []resource.Port{{Name: "redis", ContainerPort: 6379, Protocol: "tcp"}}
	w.Mounts[0].ReadOnly = true
	return rs
}

func callIndex(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func TestExecuteReplace(t *testing.T) {
	r := newRequest(t, 0)
	r.req.Resources = replaceable(manifest(0))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.rt.Calls = nil
	r.req.Revision = 2
	r.req.Resources = replaceable(manifest(0))
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	calls := r.rt.Calls
	order := []string{
		"CreateWorkload:cache-redis-next", "StartWorkload:cache-redis-next",
		"StopWorkload:cache-redis", "DeleteWorkload:cache-redis", "RenameWorkload:cache-redis-next->cache-redis",
	}
	for i := 1; i < len(order); i++ {
		if a, b := callIndex(calls, order[i-1]), callIndex(calls, order[i]); a < 0 || b < 0 || a > b {
			t.Fatalf("%s must come before %s: %v", order[i-1], order[i], calls)
		}
	}
	w, ok := r.rt.Workloads["cache-redis"]
	// No Service targets it, so it has no traffic to switch and no alias.
	if !ok || w.Request.Image != "redis:7.2" || w.State != ports.StateRunning || len(w.Aliases["cache-net"]) != 0 {
		t.Fatalf("after replace: %+v", w)
	}
	if _, left := r.rt.Workloads["cache-redis-next"]; left {
		t.Fatal("-next container left behind")
	}
	if r.tr.applied[Key{resource.KindWorkload, "redis"}].Revision != 2 {
		t.Fatalf("applied state not updated: %+v", r.tr.applied)
	}
}

func TestExecuteReplaceFailureKeepsOldRunning(t *testing.T) {
	r := newRequest(t, 0)
	r.req.Resources = replaceable(manifest(0))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	before := r.tr.applied[Key{resource.KindWorkload, "redis"}]
	r.rt.Calls = nil
	r.rt.Health = ports.HealthUnhealthy
	r.req.Revision, r.req.Timeout = 2, 50*time.Millisecond
	r.req.Resources = replaceable(manifest(0))
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"

	err := r.run()
	var execErr *ExecError
	if !errors.As(err, &execErr) || execErr.Disrupted || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("want non-disruptive ExecError, got %v", err)
	}
	if slices.Contains(r.rt.Calls, "StopWorkload:cache-redis") || slices.ContainsFunc(r.rt.Calls, func(c string) bool { return strings.HasPrefix(c, "ConnectNetwork") }) {
		t.Fatalf("old container was touched: %v", r.rt.Calls)
	}
	if old := r.rt.Workloads["cache-redis"]; old.State != ports.StateRunning || old.Request.Image != "redis:7.4" {
		t.Fatalf("old container: %+v", old)
	}
	if next := r.rt.Workloads["cache-redis-next"]; next == nil || next.State != ports.StateExited {
		t.Fatalf("failed -next should be stopped and kept: %+v", next)
	}
	if got := r.tr.applied[Key{resource.KindWorkload, "redis"}]; got.Digest != before.Digest {
		t.Fatal("applied state must keep the previous workload")
	}
	if last := r.tr.steps[len(r.tr.steps)-1]; last.Status != StepKept {
		t.Fatalf("last step: %+v", last)
	}

	// The next attempt cleans up the leftover -next and succeeds.
	r.rt.Health = ""
	r.req.Revision, r.req.Timeout = 3, time.Second
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if r.rt.Workloads["cache-redis"].Request.Image != "redis:7.2" {
		t.Fatalf("retry: %+v", r.rt.Workloads["cache-redis"])
	}
}

func TestExecuteRecreateFailureIsDisruptive(t *testing.T) {
	r := newRequest(t, freePort(t))
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.rt.Health = ports.HealthUnhealthy
	r.req.Revision, r.req.Timeout = 2, 50*time.Millisecond
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	var execErr *ExecError
	if err := r.run(); !errors.As(err, &execErr) || !execErr.Disrupted {
		t.Fatalf("want disruptive ExecError, got %v", err)
	}
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	rt := fakeruntime.New("img")
	lbl := map[string]string{ports.LabelManaged: "true", ports.LabelRelease: "web"}
	create := func(name string) {
		if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: name, Image: "img", Labels: lbl}); err != nil {
			t.Fatal(err)
		}
	}
	applied := []release.AppliedResource{{Kind: resource.KindWorkload, Name: "app", RuntimeName: "web-app", Digest: "sha256:x"}}

	// Both present: the unfinished new container is removed.
	create("web-app")
	create("web-app-next")
	repaired, err := testEnv.RepairReplace(ctx, rt, t.TempDir(), "web", applied)
	if err != nil || len(repaired) != 0 || rt.Workloads["web-app-next"] != nil || rt.Workloads["web-app"] == nil {
		t.Fatalf("both present: %v %v %v", repaired, err, rt.Workloads)
	}

	// Old already removed: -next takes the canonical name and is marked for re-apply.
	if err := rt.DeleteWorkload(ctx, "web-app"); err != nil {
		t.Fatal(err)
	}
	create("web-app-next")
	repaired, err = testEnv.RepairReplace(ctx, rt, t.TempDir(), "web", applied)
	if err != nil || len(repaired) != 1 || repaired[0].Digest != "" || rt.Workloads["web-app"] == nil {
		t.Fatalf("only -next: %v %v %v", repaired, err, rt.Workloads)
	}
}

func TestFailedReplaceWaitsForEarlierRecreates(t *testing.T) {
	r := newRequest(t, freePort(t))
	port := r.req.Resources[0].Spec.(*resource.WorkloadSpec).Ports[0].ContainerPort
	noService := func() []resource.Resource { rs := withWorkload(manifest(port), "web", 8080); return rs[:len(rs)-1] }
	r.req.Resources = noService()
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	// redis (recreate) comes first in the plan, then web (replace), whose new container fails.
	r.rt.Calls = nil
	r.req.Revision = 2
	r.req.Resources = noService()
	r.req.Resources[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	r.req.Resources[len(r.req.Resources)-1].Spec.(*resource.WorkloadSpec).Image = "app:2"
	r.rt.Fail["StartWorkload:cache-web-next"] = errors.New("boom")
	r.rt.Health = ports.HealthUnhealthy
	err := r.run()
	if err == nil {
		t.Fatal("expected failure")
	}
	// Stopped once by its recreate, and once more because it never became ready.
	last := slices.Index(r.rt.Calls, "StartWorkload:cache-web-next")
	if last < 0 || !slices.Contains(r.rt.Calls[last:], "StopWorkload:cache-redis") {
		t.Fatalf("recreated redis not handled after the failed replace:\n%s", strings.Join(r.rt.Calls, "\n"))
	}
	if w := r.rt.Workloads["cache-redis"]; w.State != ports.StateExited {
		t.Fatalf("unready recreated workload must be stopped: %+v", w.State)
	}
	if w := r.rt.Workloads["cache-web"]; w.State != ports.StateRunning {
		t.Fatalf("previous web must keep running: %v", w.State)
	}
}

func TestWorkloadSecurityRequest(t *testing.T) {
	s := manifest(0)[0].Spec.(*resource.WorkloadSpec)
	wr, err := workloadRequest("cache", "redis", s, map[string]string{}, map[Key]string{{resource.KindConfig, "conf"}: "/x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wr.NoNewPrivileges || wr.ReadOnlyRootfs {
		t.Fatalf("defaults: %+v", wr)
	}
	off := false
	s.Security = &resource.Security{NoNewPrivileges: &off, ReadOnlyRootFilesystem: true, CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN"}, User: "999:999"}
	wr, err = workloadRequest("cache", "redis", s, map[string]string{}, map[Key]string{{resource.KindConfig, "conf"}: "/x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if wr.NoNewPrivileges || !wr.ReadOnlyRootfs || wr.Tmpfs["/tmp"] == "" || wr.CapDrop[0] != "ALL" || wr.CapAdd[0] != "CHOWN" || wr.User != "999:999" {
		t.Fatalf("security: %+v", wr)
	}
}
