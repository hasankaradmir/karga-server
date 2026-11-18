package releases

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"
	"github.com/hasankaradmir/karga-server/internal/ports"
	"github.com/hasankaradmir/karga-server/internal/ports/runtimetest"
)

const redisPkg = "../../../tests/testdata/redis"

// Tests run without Envoy: proxy and gateway configuration is written, readiness is assumed.

type env struct {
	t    *testing.T
	svc  *Service
	rt   *fakeruntime.Runtime
	port string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	store, err := filestore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fakeruntime.New().WithGateway()
	host := &proxyops.Host{Proxies: fakeruntime.Proxies{}, Net: hostnet.Net{}}
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	host.UseRuntime(info)
	svc, err := New(Deps{Store: store, Runtime: rt, Host: host, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, svc: svc, rt: rt, port: strconv.Itoa(runtimetest.FreePort(t))}
}

func (e *env) opts(sets ...string) Values {
	return Values{Sets: packages.Sets(append([]string{"service.enabled=false"}, sets...)...)}
}

func common() CommonOptions {
	return CommonOptions{Timeout: time.Second}
}

func (e *env) install(sets ...string) (*Result, error) {
	return e.svc.Lifecycle.Install(context.Background(), InstallRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts(sets...)}, nil)
}

func (e *env) upgrade(sets ...string) (*Result, error) {
	return e.svc.Lifecycle.Upgrade(context.Background(), UpgradeRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts(sets...)}, nil)
}

func (e *env) release() release.Release {
	e.t.Helper()
	r, err := e.svc.c.store.GetRelease(context.Background(), "cache")
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) image() string { return e.rt.Workloads["cache-redis"].Request.Image }

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func TestInstallIdempotentAndConflicts(t *testing.T) {
	e := newEnv(t)
	res := must[*Result](t)(e.install())
	if res.Revision != 1 || res.Status != string(release.StatusDeployed) {
		t.Fatalf("install: %+v", res)
	}
	if r := e.release(); r.CurrentRevision != 1 || r.Status != release.StatusDeployed || r.PackageVersion != "1.0.0" {
		t.Fatalf("release: %+v", r)
	}
	if res := must[*Result](t)(e.install()); !res.NoOp {
		t.Fatalf("same install should be a no-op: %+v", res)
	}
	if _, err := e.install("image.tag=7.2"); !errors.Is(err, ErrExists) {
		t.Fatalf("different values: want ErrExists, got %v", err)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.Lifecycle.Install(context.Background(), InstallRequest{CommonOptions: CommonOptions{DryRun: true}, Name: "cache", Package: redisPkg, Values: e.opts()}, nil)
	if err != nil || !res.DryRun || !changed(res.Plan) {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if _, err := e.svc.c.store.GetRelease(context.Background(), "cache"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("dry run created a release: %v", err)
	}
	if len(e.rt.Calls) != 1 || e.rt.Calls[0] != "Info:" { // reading runtime info is all it may do
		t.Fatalf("dry run touched the runtime: %v", e.rt.Calls)
	}
}

func TestUpgradeFailRollback(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	res := must[*Result](t)(e.upgrade("image.tag=7.2"))
	if res.Revision != 2 || res.From != 1 || e.image() != "redis:7.2" {
		t.Fatalf("upgrade: %+v image %s", res, e.image())
	}

	// Failed upgrade: revision 3 failed, current stays 2, container stopped but kept.
	e.rt.Health = ports.HealthUnhealthy
	if _, err := e.upgrade("image.tag=7.0"); err == nil {
		t.Fatal("expected failed upgrade")
	}
	rel := e.release()
	if rel.Status != release.StatusFailed || rel.CurrentRevision != 2 {
		t.Fatalf("after failure: %+v", rel)
	}
	rev3 := must[release.Revision](t)(e.svc.c.store.GetRevision(context.Background(), "cache", 3))
	if rev3.Status != release.RevisionFailed || !strings.Contains(rev3.Description, "unhealthy") {
		t.Fatalf("revision 3: %+v", rev3)
	}
	if w := e.rt.Workloads["cache-redis"]; w.State != ports.StateExited {
		t.Fatalf("failed container should be stopped, got %+v", w)
	}

	// Rollback without a revision goes back to the last good one (2).
	e.rt.Health = ""
	res = must[*Result](t)(e.svc.Lifecycle.Rollback(context.Background(), RollbackRequest{CommonOptions: common(), Name: "cache"}, nil))
	if res.Revision != 4 || e.image() != "redis:7.2" || e.rt.Workloads["cache-redis"].State != ports.StateRunning {
		t.Fatalf("rollback: %+v image %s", res, e.image())
	}
	hist := must[[]release.Revision](t)(e.svc.Reads.History(context.Background(), "cache"))
	var got []string
	for _, r := range hist {
		got = append(got, string(r.Status)+":"+r.PackageVersion+":"+r.Description)
	}
	if len(hist) != 4 || hist[3].Description != "Rollback to 2" || hist[2].Status != release.RevisionFailed {
		t.Fatalf("history: %v", got)
	}
	// Only the newest deployed revision shows as deployed; the state keeps the others deployed, so
	// an explicit rollback to one of them still works (below).
	if hist[0].Status != release.RevisionSuperseded || hist[1].Status != release.RevisionSuperseded || hist[3].Status != release.RevisionDeployed {
		t.Fatalf("history statuses: %v", got)
	}

	// Explicit rollback to revision 1.
	must[*Result](t)(e.svc.Lifecycle.Rollback(context.Background(), RollbackRequest{CommonOptions: common(), Name: "cache", Revision: 1}, nil))
	if e.image() != "redis:7.4" {
		t.Fatalf("rollback to 1: image %s", e.image())
	}
	if _, err := e.svc.Lifecycle.Rollback(context.Background(), RollbackRequest{CommonOptions: common(), Name: "cache", Revision: 3}, nil); err == nil {
		t.Fatal("rollback to a failed revision must be refused")
	}
}

func TestUpgradeReuseValues(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install("config.maxmemory=64mb"))
	must[*Result](t)(e.svc.Lifecycle.Upgrade(context.Background(), UpgradeRequest{CommonOptions: common(), Name: "cache", Package: redisPkg,
		Values: Values{Sets: packages.Sets([]string{"image.tag=7.2"}...)}, ReuseValues: true}, nil))
	vals := must[map[string]any](t)(e.svc.Reads.Values(context.Background(), "cache", 0, false))
	if vals["config"].(map[string]any)["maxmemory"] != "64mb" || vals["image"].(map[string]any)["tag"] != "7.2" {
		t.Fatalf("reused values: %#v", vals)
	}
	all := must[map[string]any](t)(e.svc.Reads.Values(context.Background(), "cache", 0, true))
	if all["image"].(map[string]any)["repository"] != "redis" {
		t.Fatalf("all values: %#v", all)
	}
}

func TestUpgradeMissingRelease(t *testing.T) {
	e := newEnv(t)
	if _, err := e.upgrade(); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	res := must[*Result](t)(e.svc.Lifecycle.Upgrade(context.Background(), UpgradeRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts(), Install: true}, nil))
	if res.Revision != 1 {
		t.Fatalf("upgrade --install: %+v", res)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestInterruptedOperation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	begin := func(pid int) {
		t.Helper()
		if _, err := e.svc.c.store.BeginOperation(ctx, release.Operation{Release: "cache", Type: "upgrade", Revision: 2,
			PID: pid, Hostname: e.svc.c.hostname, StartedAt: time.Now()}, func(release.Operation) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	// Another process that is alive. One with this process's PID is from an earlier process: stale.
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	begin(live.Process.Pid)
	if _, err := e.upgrade("image.tag=7.2"); !errors.Is(err, ErrLocked) {
		t.Fatalf("live lock: want ErrLocked, got %v", err)
	}
	force := common()
	force.Force = true
	if _, err := e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: force, Name: "cache", Package: redisPkg, Values: e.opts("image.tag=7.2")}, nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("--force must not break a live operation: %v", err)
	}

	begin(deadPID(t))
	if _, err := e.upgrade("image.tag=7.2"); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("dead process: want ErrInterrupted, got %v", err)
	}
	rep := must[*StatusReport](t)(e.svc.Reads.Status(ctx, "cache"))
	if rep.Interrupted == "" {
		t.Fatal("status should report the interrupted operation")
	}
	if _, err := e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: force, Name: "cache", Package: redisPkg, Values: e.opts("image.tag=7.2")}, nil); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	for _, tag := range []string{"7.0", "7.1", "7.2", "7.3"} {
		must[*Result](t)(e.svc.Lifecycle.Upgrade(context.Background(), UpgradeRequest{
			CommonOptions: CommonOptions{Timeout: time.Second, HistoryMax: new(2)}, Name: "cache", Package: redisPkg, Values: e.opts("image.tag=" + tag)}, nil))
	}
	hist := must[[]release.Revision](t)(e.svc.Reads.History(context.Background(), "cache"))
	var nums []int
	for _, r := range hist {
		nums = append(nums, r.Number)
	}
	// Revision 1 holds the Config/Volume/Network rows still applied (unchanged since install), so it stays.
	if !slices.Equal(nums, []int{1, 4, 5}) {
		t.Fatalf("revisions after prune: %v", nums)
	}
}

func TestUninstall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	if _, err := e.svc.Lifecycle.Uninstall(ctx, UninstallRequest{Name: "cache", KeepHistory: true}); err != nil {
		t.Fatal(err)
	}
	if r := e.release(); r.Status != release.StatusUninstalled {
		t.Fatalf("keep-history: %+v", r)
	}
	if _, ok := e.rt.Volumes["cache-redis-data"]; !ok || len(e.rt.Workloads) != 0 {
		t.Fatalf("volume must survive, workloads must go: %v %v", e.rt.Volumes, e.rt.Workloads)
	}
	// Reinstall after uninstall continues the revision numbers and reuses the volume.
	res := must[*Result](t)(e.install())
	if res.Revision != 2 {
		t.Fatalf("reinstall: %+v", res)
	}
	if _, err := e.svc.Lifecycle.Uninstall(ctx, UninstallRequest{Name: "cache", DeleteVolumes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.c.store.GetRelease(ctx, "cache"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("release should be gone: %v", err)
	}
	if len(e.rt.Volumes) != 0 {
		t.Fatalf("--delete-volumes left: %v", e.rt.Volumes)
	}
}

func TestManifestAndStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "db", Package: "../../../tests/testdata/postgres",
		Values: Values{Sets: packages.Sets([]string{"service.enabled=false", "database.password=hunter2"}...)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rs := must[[]resource.Resource](t)(e.svc.Reads.Manifest(ctx, "db", 0))
	for _, r := range rs {
		if s, ok := r.Spec.(*resource.SecretSpec); ok && s.Values["password"] != "<redacted>" {
			t.Fatalf("manifest secret: %v", s.Values)
		}
	}
	rev := must[release.Revision](t)(e.svc.c.store.GetRevision(ctx, "db", 1))
	if strings.Contains(string(rev.Manifest), "hunter2") {
		t.Fatal("stored manifest contains the secret")
	}

	rep := must[*StatusReport](t)(e.svc.Reads.Status(ctx, "db"))
	var workload ResourceStatus
	for _, r := range rep.Resources {
		if r.Kind == resource.KindWorkload {
			workload = r
		}
	}
	if workload.Actual != ports.StateRunning || workload.Health != ports.HealthHealthy || len(rep.Orphans) != 0 {
		t.Fatalf("status: %+v", rep)
	}
	delete(e.rt.Workloads, "db-postgres")
	rep = must[*StatusReport](t)(e.svc.Reads.Status(ctx, "db"))
	for _, r := range rep.Resources {
		if r.Kind == resource.KindWorkload && r.Actual != "missing" {
			t.Fatalf("deleted workload not reported missing: %+v", r)
		}
	}
}

const webPkg = "../../../tests/integration/testdata/web"

func TestReplaceUpgradeAndNonDisruptiveFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	up := func(sets ...string) (*Result, error) {
		return e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "site", Package: webPkg, Values: Values{Sets: packages.Sets(sets...)}}, nil)
	}
	if _, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "site", Package: webPkg}, nil); err != nil {
		t.Fatal(err)
	}
	res := must[*Result](t)(up("content=v2"))
	var w deploymentChange
	for _, c := range res.Plan {
		if c.Kind == resource.KindWorkload {
			w = deploymentChange{c.Action, c.Strategy}
		}
	}
	if w != (deploymentChange{"update", "replace"}) || !slices.Contains(e.rt.Calls, "RenameWorkload:site-web-next->site-web") {
		t.Fatalf("replace upgrade: %+v %v", w, e.rt.Calls)
	}

	// A failed replace changes nothing that runs: the release stays deployed at revision 2.
	e.rt.Health = ports.HealthUnhealthy
	_, err := up("content=v3")
	if err == nil || !strings.Contains(err.Error(), "revision 2 is still serving") {
		t.Fatalf("failed replace: %v", err)
	}
	r := must[release.Release](t)(e.svc.c.store.GetRelease(ctx, "site"))
	if r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed replace: %+v", r)
	}
	if rev := must[release.Revision](t)(e.svc.c.store.GetRevision(ctx, "site", 3)); rev.Status != release.RevisionFailed {
		t.Fatalf("revision 3: %+v", rev)
	}
	if e.rt.Workloads["site-web"].State != ports.StateRunning {
		t.Fatal("old container must keep running")
	}

	// --strategy recreate overrides the spec.
	e.rt.Health = ""
	res = must[*Result](t)(e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "site", Package: webPkg,
		Values: Values{Sets: packages.Sets([]string{"content=v4"}...)}, Strategy: "recreate"}, nil))
	for _, c := range res.Plan {
		if c.Kind == resource.KindWorkload && c.Strategy != "recreate" {
			t.Fatalf("override ignored: %+v", c)
		}
	}
}

type deploymentChange struct{ action, strategy string }

func TestReconcileBeforePlan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "site", Package: webPkg}, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the old container was removed but before the rename.
	if err := e.rt.RenameWorkload(ctx, "site-web", "site-web-next"); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "site", Package: webPkg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.rt.Workloads["site-web"]; !ok || e.rt.Workloads["site-web-next"] != nil {
		t.Fatalf("not repaired: %v", e.rt.Workloads)
	}
	if !changed(res.Plan) {
		t.Fatal("repaired workload should be applied again")
	}
}

const pgPkg = "../../../tests/testdata/postgres"

func TestWriteOnlyValuesNotStored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pg := func(sets ...string) Values {
		return Values{Sets: packages.Sets(append([]string{"service.enabled=false"}, sets...)...)}
	}
	if _, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "db", Package: pgPkg, Values: pg("database.password=hunter2")}, nil); err != nil {
		t.Fatal(err)
	}
	rev := must[release.Revision](t)(e.svc.c.store.GetRevision(ctx, "db", 1))
	for _, v := range []map[string]any{rev.Values, rev.ValuesAll} {
		if v["database"].(map[string]any)["password"] != "<redacted>" {
			t.Fatalf("stored values: %#v", v)
		}
	}
	shown := must[map[string]any](t)(e.svc.Reads.Values(ctx, "db", 0, true))
	if shown["database"].(map[string]any)["password"] != "<redacted>" {
		t.Fatalf("get values --all: %#v", shown)
	}

	// Same values: no-op. A new password is a change even though both are stored as <redacted>.
	if res := must[*Result](t)(e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "db", Package: pgPkg, Values: pg("database.password=hunter2")}, nil)); !res.NoOp {
		t.Fatalf("same password should be a no-op: %+v", res)
	}
	if _, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "db", Package: pgPkg, Values: pg("database.password=other")}, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("changed password: want ErrExists, got %v", err)
	}

	// --reuse-values asks for writeOnly values again.
	_, err := e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "db", Package: pgPkg, ReuseValues: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "database.password is writeOnly") {
		t.Fatalf("reuse without password: %v", err)
	}
	if _, err := e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "db", Package: pgPkg, ReuseValues: true,
		Values: Values{Sets: packages.Sets([]string{"database.password=hunter3"}...)}}, nil); err != nil {
		t.Fatalf("reuse with password: %v", err)
	}
	vals := must[map[string]any](t)(e.svc.Reads.Values(ctx, "db", 0, false))
	if vals["service"].(map[string]any)["enabled"] == nil {
		t.Fatalf("reused values lost: %#v", vals)
	}
}

func TestStateKeyAndKeyedDigests(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: CommonOptions{}, Name: "cache", Package: redisPkg, Values: e.opts()}, nil); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(e.svc.c.home, "state.key")
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("state key: %v %v", info, err)
	}
	rev, err := e.svc.c.store.GetRevision(ctx, "cache", 1)
	if err != nil || !strings.HasPrefix(rev.ValuesDigest, "hmac-sha256:") {
		t.Fatalf("values digest not keyed: %q %v", rev.ValuesDigest, err)
	}
	// Another key gives another digest; the same key the same one.
	a := e.svc.c.env.Digest([]byte("pw"))
	with := func(key []byte) *deployment.Env {
		return deployment.NewEnv(e.svc.c.env.Host(), deployment.EnvConfig{DigestKey: key})
	}
	if with([]byte("another-key-of-16b")).Digest([]byte("pw")) == a {
		t.Fatal("digest does not depend on the key")
	}
	key, _ := os.ReadFile(keyPath)
	if with(key).Digest([]byte("pw")) != a {
		t.Fatal("same key, different digest")
	}
}

func TestStateKeyCreatedOnceUnderConcurrency(t *testing.T) {
	home := t.TempDir()
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := loadKey(home)
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent key creation: %v", err)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 {
		t.Fatalf("leftover temporary files: %v", entries)
	}
}

func TestFailedInstallLeavesNothing(t *testing.T) {
	e := newEnv(t)
	e.rt.Fail["EnsureImage:redis:7.4"] = errors.New("image not found")
	_, err := e.install("image.tag=7.4")
	if err == nil || !strings.Contains(err.Error(), "nothing was installed") {
		t.Fatalf("install: %v", err)
	}
	if _, err := e.svc.c.store.GetRelease(context.Background(), "cache"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("failed install left a release: %v", err)
	}
	delete(e.rt.Fail, "EnsureImage:redis:7.4")
	if res := must[*Result](t)(e.install("image.tag=7.4")); res.Revision != 1 {
		t.Fatalf("reinstall: %+v", res)
	}
}

func TestUpgradeRecreatesWhatWasRemoved(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	if err := e.rt.DeleteWorkload(context.Background(), "cache-redis"); err != nil {
		t.Fatal(err)
	}
	res := must[*Result](t)(e.upgrade())
	var created []string
	for _, c := range res.Plan {
		if c.Action == deployment.ActionCreate {
			created = append(created, c.Kind+"/"+c.Name)
		}
	}
	if !slices.Equal(created, []string{"Workload/redis"}) || e.rt.Workloads["cache-redis"] == nil {
		t.Fatalf("plan created %v, workload %v", created, e.rt.Workloads["cache-redis"])
	}
}

func TestAnonymousVolumesSurviveUpgrades(t *testing.T) {
	e := newEnv(t)
	e.rt.ImageVolumes = map[string][]string{"redis:7.2": {"/cache"}}
	res := must[*Result](t)(e.install("image.tag=7.2"))
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "keeps image VOLUME /cache in an anonymous volume") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	anon := func() string {
		for _, m := range e.rt.Workloads["cache-redis"].Request.Mounts {
			if m.Target == "/cache" {
				return m.Source
			}
		}
		return ""
	}
	first := anon()
	if first == "" {
		t.Fatal("no anonymous volume at /cache")
	}

	// A recreate moves the anonymous volume to the new container instead of deleting it.
	e.rt.ImageVolumes["redis:7.4"] = []string{"/cache"}
	must[*Result](t)(e.upgrade("image.tag=7.4"))
	if e.image() != "redis:7.4" || anon() != first {
		t.Fatalf("after upgrade: image %s, /cache on %q, want %q", e.image(), anon(), first)
	}
	if _, ok := e.rt.Volumes[first]; !ok {
		t.Fatal("anonymous volume was deleted by the upgrade")
	}

	// Uninstall deletes it: nothing could mount it again.
	if _, err := e.svc.Lifecycle.Uninstall(context.Background(), UninstallRequest{Name: "cache"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.rt.Volumes[first]; ok {
		t.Fatal("uninstall kept the anonymous volume")
	}
}

// scratchPkg writes a package whose volume "scratch" (retention delete) exists while .Values.scratch is true.
func scratchPkg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"Package.yaml": "apiVersion: karga.io/v1alpha1\nname: scratch\nversion: 0.1.0\nruntime:\n  supported: [docker]\n",
		"values.yaml":  "scratch: true\n",
		"templates/r.yaml": `{{- if .Values.scratch }}
apiVersion: karga.io/v1alpha1
kind: Volume
metadata: {name: scratch}
spec: {retention: delete}
---
{{- end }}
apiVersion: karga.io/v1alpha1
kind: Workload
metadata: {name: app}
spec:
  image: busybox:1.37
  command: [sleep, "300"]
`,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestUpgradeDeletingVolumeNeedsFlag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pkgDir := scratchPkg(t)
	must[*Result](t)(e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "s", Package: pkgDir}, nil))
	without := UpgradeRequest{CommonOptions: common(), Name: "s", Package: pkgDir, Values: Values{Sets: packages.Sets([]string{"scratch=false"}...)}}

	dry := without
	dry.DryRun = true
	res := must[*Result](t)(e.svc.Lifecycle.Upgrade(ctx, dry, nil))
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "needs --delete-volumes") {
		t.Fatalf("dry-run warnings: %v", res.Warnings)
	}

	if _, err := e.svc.Lifecycle.Upgrade(ctx, without, nil); err == nil || !strings.Contains(err.Error(), "deletes volume scratch and its data") {
		t.Fatalf("upgrade without --delete-volumes: %v", err)
	}
	if rel := must[release.Release](t)(e.svc.c.store.GetRelease(ctx, "s")); rel.CurrentRevision != 1 {
		t.Fatalf("refused upgrade changed the release: %+v", rel)
	}
	if _, ok := e.rt.Volumes["s-scratch"]; !ok {
		t.Fatal("refused upgrade changed something")
	}

	with := without
	with.DeleteVolumes = true
	must[*Result](t)(e.svc.Lifecycle.Upgrade(ctx, with, nil))
	if _, ok := e.rt.Volumes["s-scratch"]; ok {
		t.Fatal("volume kept with --delete-volumes")
	}
}

// Commands on one release run one after the other, from planning to the end: each gets its own
// revision and plans against what the previous one applied.
func TestConcurrentUpgradesSerialize(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install("image.tag=7.0"))
	tags := []string{"7.1", "7.2", "7.3", "7.4"}
	errs := make(chan error, len(tags))
	for _, tag := range tags {
		go func() {
			_, err := e.upgrade("image.tag=" + tag)
			errs <- err
		}()
	}
	for range tags {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	hist := must[[]release.Revision](t)(e.svc.Reads.History(context.Background(), "cache"))
	if len(hist) != 1+len(tags) {
		t.Fatalf("revisions: %+v", hist)
	}
	for i, r := range hist {
		want := release.RevisionSuperseded // every upgrade succeeded; only the last one runs
		if i == len(hist)-1 {
			want = release.RevisionDeployed
		}
		if r.Number != i+1 || r.Status != want {
			t.Fatalf("revision %d: %s %s", i+1, r.Status, r.Description)
		}
	}
	if rel := e.release(); rel.Status != release.StatusDeployed || rel.CurrentRevision != 1+len(tags) {
		t.Fatalf("release: %+v", rel)
	}
}

// A user's command waits for the release; giving up waiting does not take it.
func TestLockHonoursContext(t *testing.T) {
	e := newEnv(t)
	unlock, err := e.svc.c.locks.lock(context.Background(), "cache")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.c.locks.tryLock("cache"); !errors.Is(err, ErrLocked) {
		t.Fatalf("try: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.svc.c.locks.lock(ctx, "cache"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	unlock()
	again, err := e.svc.c.locks.tryLock("cache")
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again()
	if n := len(e.svc.c.locks.busy); n != 0 {
		t.Fatalf("%d lock entries left for releases nobody holds", n)
	}
}

// changed reports whether a plan touches the runtime.
func changed(plan []deployment.Summary) bool {
	for _, c := range plan {
		if c.Action != deployment.ActionUnchanged {
			return true
		}
	}
	return false
}

// A restarted server masks the Secret values of the releases it has before applying any of them.
func TestRegisterSecretsAfterRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "db", Package: "../../../tests/testdata/postgres",
		Values: Values{Sets: packages.Sets([]string{"service.enabled=false", "database.password=hunter2-restart"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	restarted, err := New(Deps{Store: e.svc.c.store, Runtime: e.svc.c.rt, Host: e.svc.c.env.Host(), Home: e.svc.c.home,
		Redact: func(v string) { got = append(got, v) }})
	if err != nil {
		t.Fatal(err)
	}
	restarted.RegisterSecrets(ctx)
	if !slices.Contains(got, "hunter2-restart") {
		t.Fatalf("registered %d values, not the password", len(got))
	}
	// A release whose revision directory is gone (pruned, or a copied state) is skipped.
	if err := os.RemoveAll(filepath.Join(e.svc.c.home, "releases")); err != nil {
		t.Fatal(err)
	}
	got = nil
	restarted.RegisterSecrets(ctx)
	if len(got) != 0 {
		t.Fatalf("registered %v without secret files", got)
	}
}

// A variable set from a Secret is visible through docker inspect: the plan says so, and the
// warnings of a successful deployment keep it.
func TestEnvironmentFromWarns(t *testing.T) {
	e := newEnv(t)
	dir := scratchPkg(t)
	tpl := `apiVersion: karga.io/v1alpha1
kind: Secret
metadata: {name: db}
spec:
  values: {uri: postgres://db}
---
apiVersion: karga.io/v1alpha1
kind: Workload
metadata: {name: api}
spec:
  image: busybox:1.37
  command: [sleep, "300"]
  environmentFrom:
    - {name: DB_URI, secret: db, key: uri}
`
	if err := os.WriteFile(filepath.Join(dir, "templates", "api.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		res, err := e.svc.Lifecycle.Install(context.Background(), InstallRequest{CommonOptions: CommonOptions{DryRun: dry, Timeout: time.Second}, Name: "envs", Package: dir}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, `"api" sets DB_URI from Secret "db"`) }) {
			t.Fatalf("dry run %v: warnings %q", dry, res.Warnings)
		}
	}
}

// A command that waits for a busy release stops waiting when its client goes away, though the
// operation's own context is never canceled (ADR-0015).
func TestLockWaitEndsWithTheClient(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	unlock, err := e.svc.c.locks.lock(context.Background(), "cache")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	client, cancel := context.WithCancel(context.Background())
	ctx := WithWait(context.WithoutCancel(client), client)
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts("image.tag=7.2")}, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want the wait to end with the client, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command kept waiting for the lock after its client went away")
	}
}
