package releases

import (
	"context"
	"errors"
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
	if err := e.rt.DeleteWorkload(ctx, "cache-redis"); err != nil {
		t.Fatal(err)
	}
	if res := reconcile(); !strings.Contains(actionsOf(res), "create:Workload/redis:done") || e.rt.Workloads["cache-redis"] == nil {
		t.Fatalf("missing container: %s", actionsOf(res))
	}
	if rel := e.release(); rel.CurrentRevision != 1 || rel.Status != release.StatusDeployed {
		t.Fatalf("reconcile changed the release: %+v", rel)
	}

	// A container stopped outside karga is started; an unhealthy one restarted.
	if err := e.rt.StopWorkload(ctx, "cache-redis", 0); err != nil {
		t.Fatal(err)
	}
	if res := reconcile(); actionsOf(res) != "start:Workload/redis:done" || e.rt.Workloads["cache-redis"].State != ports.StateRunning {
		t.Fatalf("stopped container: %s", actionsOf(res))
	}
	e.rt.Health = ports.HealthUnhealthy
	if res := reconcile(); actionsOf(res) != "restart:Workload/redis:done" {
		t.Fatalf("unhealthy container: %s", actionsOf(res))
	}
	e.rt.Health = ""

	// A container created from another spec (changed outside karga) is recreated.
	e.rt.Workloads["cache-redis"].Request.Labels[ports.LabelDigest] = "sha256:other"
	if res := reconcile(); !strings.Contains(actionsOf(res), "create:Workload/redis:done") {
		t.Fatalf("changed container: %s", actionsOf(res))
	}

	// A repair interrupted while the new container got ready leaves it failed; the next repair that
	// gets it running records it as applied again.
	if err := e.rt.DeleteWorkload(ctx, "cache-redis"); err != nil {
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
	ops := must[[]release.Operation](t)(e.svc.Reads.Events(ctx, "cache", 0, 10))
	if len(ops) < 4 || ops[0].Type != ReconcileOperation {
		t.Fatalf("events: %+v", ops)
	}
	st := must[*StatusReport](t)(e.svc.Reads.Status(ctx, "cache"))
	if st.Reconcile == nil || st.Reconcile.Status != string(release.OperationSucceeded) {
		t.Fatalf("status: %+v", st.Reconcile)
	}
}

func TestReconcileLeavesAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	if err := e.rt.StopWorkload(ctx, "cache-redis", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Lifecycle.Pause(ctx, "cache", true); err != nil {
		t.Fatal(err)
	}
	if res := must[*ReconcileResult](t)(e.svc.Lifecycle.Repair(ctx, "cache", false)); res.Skipped != "paused" || e.rt.Workloads["cache-redis"].State == ports.StateRunning {
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
	e.svc.Now = func() time.Time { return now }
	if err := e.rt.StopWorkload(ctx, "cache-redis", 0); err != nil {
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
	if err := e.rt.StopWorkload(ctx, "cache-redis", 0); err != nil {
		t.Fatal(err)
	}
	e.rt.Fail = map[string]error{"StartWorkload:cache-redis": errors.New("busy")}
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
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
