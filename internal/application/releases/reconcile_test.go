package releases

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func actionsOf(res *ReconcileResult) string {
	var out []string
	for _, a := range res.Actions {
		out = append(out, a.Action+":"+a.Kind+"/"+a.Name+":"+a.Status)
	}
	return strings.Join(out, ",")
}

func TestReconcileRepairs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	reconcile := func() *ReconcileResult {
		t.Helper()
		return must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", true))
	}
	if res := reconcile(); len(res.Actions) != 0 || res.Skipped != "" {
		t.Fatalf("nothing to do: %+v", res)
	}

	// A removed container is created again from the current revision, without a new revision.
	if err := e.rt.DeleteWorkload(ctx, e.rt.Name("cache-redis")); err != nil {
		t.Fatal(err)
	}
	if res := reconcile(); !strings.Contains(actionsOf(res), "create:Workload/redis:done") || e.rt.Workload("cache-redis") == nil {
		t.Fatalf("missing container: %s", actionsOf(res))
	}
	if rel := e.release(); rel.CurrentRevision != 1 || rel.Status != release.StatusDeployed {
		t.Fatalf("reconcile changed the release: %+v", rel)
	}

	// A container stopped outside karga is started; an unhealthy one restarted.
	if err := e.rt.StopWorkload(ctx, e.rt.Name("cache-redis"), 0); err != nil {
		t.Fatal(err)
	}
	if res := reconcile(); actionsOf(res) != "start:Workload/redis:done" || e.rt.Workload("cache-redis").State != ports.StateRunning {
		t.Fatalf("stopped container: %s", actionsOf(res))
	}
	e.rt.Health = ports.HealthUnhealthy
	if res := reconcile(); actionsOf(res) != "restart:Workload/redis:done" {
		t.Fatalf("unhealthy container: %s", actionsOf(res))
	}
	e.rt.Health = ""

	// A container created from another spec (changed outside karga) is recreated.
	e.rt.Workload("cache-redis").Request.Labels[ports.LabelDigest] = "sha256:other"
	if res := reconcile(); !strings.Contains(actionsOf(res), "create:Workload/redis:done") {
		t.Fatalf("changed container: %s", actionsOf(res))
	}

	// A repair interrupted while the new container got ready leaves it failed; the next repair that
	// gets it running records it as applied again.
	if err := e.rt.DeleteWorkload(ctx, e.rt.Name("cache-redis")); err != nil {
		t.Fatal(err)
	}
	e.rt.Health = ports.HealthUnhealthy
	if res := reconcile(); !strings.Contains(actionsOf(res), "wait:Workload/redis:failed") {
		t.Fatalf("interrupted repair: %s", actionsOf(res))
	}
	e.rt.Health = ""
	if res := reconcile(); actionsOf(res) != "start:Workload/redis:done" {
		t.Fatalf("second repair: %s", actionsOf(res))
	}
	for _, r := range must[*StatusReport](t)(e.svc.Reads.Status(ctx, "cache")).Resources {
		if r.Kind == "Workload" && r.Desired != "applied" {
			t.Fatalf("repaired workload still %s", r.Desired)
		}
	}

	// Every repair is an event.
	ops := must[[]release.Operation](t)(e.svc.Reads.Operations(ctx, "cache", 0, 10))
	if len(ops) < 4 || ops[0].Type != ReconcileOperation {
		t.Fatalf("events: %+v", ops)
	}
	st := must[*StatusReport](t)(e.svc.Reads.Status(ctx, "cache"))
	if st.Reconcile == nil || st.Reconcile.Status != string(release.OperationSucceeded) {
		t.Fatalf("status: %+v", st.Reconcile)
	}
}

// A Volume missing in the runtime (removed, or a state restored on another host) is not created
// empty by a repair: its files are gone, and its workloads would start on nothing. The repair fails
// and says how to go on; --empty-volumes creates it.
func TestReconcileDoesNotCreateAMissingVolume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	if err := e.rt.DeleteWorkload(ctx, e.rt.Name("cache-redis")); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.DeleteVolume(ctx, "cache-redis-data"); err != nil {
		t.Fatal(err)
	}
	res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", true))
	if actionsOf(res) != "create:Volume/redis-data:failed" || !strings.Contains(res.Actions[0].Error, "'karga reconcile cache --empty-volumes'") {
		t.Fatalf("repair: %s %+v", actionsOf(res), res.Actions)
	}
	if _, ok := e.rt.Volumes["cache-redis-data"]; ok || e.rt.Workload("cache-redis") != nil {
		t.Fatal("the volume or its workload was created")
	}
	st := must[*StatusReport](t)(e.svc.Reads.Status(ctx, "cache"))
	if st.Reconcile == nil || st.Reconcile.Status != string(release.OperationFailed) {
		t.Fatalf("status: %+v", st.Reconcile)
	}
	res = must[*ReconcileResult](t)(e.svc.Lifecycle.RepairEmptyVolumes(ctx, "cache"))
	if !strings.Contains(actionsOf(res), "create:Volume/redis-data:done") || !strings.Contains(actionsOf(res), "create:Workload/redis:done") {
		t.Fatalf("--empty-volumes: %s", actionsOf(res))
	}
}

func TestReconcileLeavesAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	if err := e.rt.StopWorkload(ctx, e.rt.Name("cache-redis"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Lifecycle.Pause(ctx, "cache", true); err != nil {
		t.Fatal(err)
	}
	if res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false)); res.Skipped != "paused" || e.rt.Workload("cache-redis").State == ports.StateRunning {
		t.Fatalf("paused: %+v", res)
	}
	if err := e.svc.Lifecycle.Pause(ctx, "cache", false); err != nil {
		t.Fatal(err)
	}

	// A failed release is the user's to fix: no automatic rollback.
	e.rt.Health = ports.HealthUnhealthy
	if _, err := e.upgrade("image.tag=7.2"); err == nil {
		t.Fatal("expected failed upgrade")
	}
	e.rt.Health = ""
	if res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false)); !strings.Contains(res.Skipped, "failed") {
		t.Fatalf("failed release: %+v", res)
	}
}

func TestReconcileBacksOffAndGivesUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	e.svc.c.now = func() time.Time { return now }
	if err := e.rt.StopWorkload(ctx, e.rt.Name("cache-redis"), 0); err != nil {
		t.Fatal(err)
	}
	e.rt.Fail["StartWorkload:cache-redis"] = errors.New("boom")
	for i := range 5 {
		res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
		if actionsOf(res) != "start:Workload/redis:failed" {
			t.Fatalf("attempt %d: %s", i, actionsOf(res))
		}
		// Too early for the next attempt: backoff.
		if res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false)); len(res.Actions) != 0 {
			t.Fatalf("attempt %d retried without backoff", i)
		}
		now = now.Add(2 * time.Minute) // past the backoff (10s doubling), within the 10-minute window
	}
	now = now.Add(-time.Minute) // 5 failures in the last 9 minutes
	res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
	if len(res.Actions) != 0 || !strings.Contains(res.GaveUp["Workload/redis"], "boom") {
		t.Fatalf("after 5 failures: %+v", res)
	}
	// karga reconcile forgets the failures.
	delete(e.rt.Fail, "StartWorkload:cache-redis")
	if res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", true)); actionsOf(res) != "start:Workload/redis:done" || res.GaveUp != nil {
		t.Fatalf("forced: %+v", res)
	}
}

// A failed repair says when it may run again, so a loop without new events comes back.
func TestNextRepair(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	if at := e.svc.Lifecycle.NextRepair("cache"); !at.IsZero() {
		t.Fatalf("nothing waits: %v", at)
	}
	if err := e.rt.StopWorkload(ctx, e.rt.Name("cache-redis"), 0); err != nil {
		t.Fatal(err)
	}
	e.rt.Fail = map[string]error{"StartWorkload:cache-redis": errors.New("busy")}
	now := time.Now()
	e.svc.c.now = func() time.Time { return now }
	must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
	if at := e.svc.Lifecycle.NextRepair("cache"); !at.Equal(now.Add(repairFirstDelay)) {
		t.Fatalf("next repair at %v, want %v", at, now.Add(repairFirstDelay))
	}
	if at := e.svc.Lifecycle.NextRepair("other"); !at.IsZero() {
		t.Fatalf("another release waits: %v", at)
	}
	e.rt.Fail = nil
	now = now.Add(repairFirstDelay)
	must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
	if at := e.svc.Lifecycle.NextRepair("cache"); !at.IsZero() {
		t.Fatalf("a successful repair waits for nothing: %v", at)
	}
}

// fullDisk fails to record an operation's end while full is set.
type fullDisk struct {
	ports.StateStore
	full *bool
}

func (f fullDisk) FinishOperation(ctx context.Context, op release.Operation) error {
	if *f.full {
		return errors.New("write operations/cache.yaml: no space left on device")
	}
	return f.StateStore.FinishOperation(ctx, op)
}

// An operation whose end could not be written (a full disk) is not taken for interrupted once there
// is room: its end is written before the release's next operation, without --force or a restart.
func TestUnrecordedEndIsWrittenLater(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	full := true
	e.svc.c.store = fullDisk{e.svc.c.store, &full}
	must[*Result](t)(e.upgrade("config.maxmemory=64mb"))
	if _, err := e.svc.c.store.RunningOperation(ctx, "cache"); err != nil {
		t.Fatalf("the upgrade's end was written: %v", err)
	}
	full = false
	if _, err := e.upgrade("config.maxmemory=32mb"); err != nil {
		t.Fatalf("after room came back: %v", err)
	}
	ops := must[[]release.Operation](t)(e.svc.Reads.Operations(ctx, "cache", 0, 3))
	if ops[0].Status != release.OperationSucceeded || ops[1].Status != release.OperationSucceeded || ops[1].Revision != 2 {
		t.Fatalf("operations: %+v / %+v", ops[0], ops[1])
	}
}

// Every operation's end is logged, with its status and duration; a failed one at WARN with its
// error, so failed deployments show in the journal.
func TestOperationEndIsLogged(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.svc.c.log = slog.New(slog.NewTextHandler(&buf, nil))
	must[*Result](t)(e.install())
	if out := buf.String(); !strings.Contains(out, `level=INFO msg="operation finished"`) || !strings.Contains(out, "type=install") ||
		!strings.Contains(out, "status=succeeded") || !strings.Contains(out, "duration=") {
		t.Fatalf("install:\n%s", out)
	}
	buf.Reset()
	e.rt.Health = ports.HealthUnhealthy
	if _, err := e.upgrade("image.tag=7.2"); err == nil {
		t.Fatal("expected a failed upgrade")
	}
	if out := buf.String(); !strings.Contains(out, `level=WARN msg="operation finished"`) || !strings.Contains(out, "status=failed") ||
		!strings.Contains(out, "error=") {
		t.Fatalf("failed upgrade:\n%s", out)
	}
}

// A container that stops again right after each repair is not restarted in a loop: each relapse
// within the window counts as a failure, the pauses grow, and after five karga gives up (SA-04).
func TestReconcileBacksOffACrashLoop(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	e.svc.c.now = func() time.Time { return now }
	starts := 0
	for range 20 {
		if err := e.rt.StopWorkload(ctx, e.rt.Name("cache-redis"), 0); err != nil {
			t.Fatal(err)
		}
		res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
		if actionsOf(res) == "start:Workload/redis:done" {
			starts++
		}
		if len(res.GaveUp) > 0 {
			break
		}
		now = now.Add(15 * time.Second)
	}
	res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
	if !strings.Contains(res.GaveUp["Workload/redis"], "failed again within 10m0s of its last repair") || starts > 6 {
		t.Fatalf("after %d starts: %+v", starts, res)
	}
	// Up for longer than the window, a later failure is repaired at once again.
	if err := e.svc.Lifecycle.Pause(ctx, "cache", false); err != nil { // forgets the failures, as karga resume does
		t.Fatal(err)
	}
	must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false))
	now = now.Add(11 * time.Minute)
	if err := e.rt.StopWorkload(ctx, e.rt.Name("cache-redis"), 0); err != nil {
		t.Fatal(err)
	}
	if res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false)); actionsOf(res) != "start:Workload/redis:done" {
		t.Fatalf("later failure: %s", actionsOf(res))
	}
}
