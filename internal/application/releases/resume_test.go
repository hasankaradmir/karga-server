package releases

import (
	"context"
	"testing"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// crash leaves cache as a karga-server that stopped while running op of revision would: the release
// in progress, the revision pending, the operation running with a process that is gone.
// crash records the operation without options.
func (e *env) crash(op string, revision int, status release.Status) {
	e.t.Helper()
	e.crashWith(op, revision, status, release.Options{})
}

// crashWith is crash for an operation started with opts.
func (e *env) crashWith(op string, revision int, status release.Status, opts release.Options) {
	e.t.Helper()
	ctx := context.Background()
	rel := e.release()
	if op == "upgrade" && revision <= rel.CurrentRevision {
		rel.CurrentRevision = revision - 1
	}
	rel.Status = status
	if err := e.svc.Store.SaveRelease(ctx, rel); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.Store.GetRevision(ctx, "cache", revision); err == nil {
		if err := e.svc.Store.UpdateRevisionStatus(ctx, "cache", revision, release.RevisionPending, "Upgrade complete"); err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.svc.Store.BeginOperation(ctx, release.Operation{Release: "cache", Type: op, Revision: revision,
		Status: release.OperationRunning, PID: 1 << 22, Hostname: e.svc.Hostname, Options: opts, StartedAt: e.svc.Now()}, nil); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) lastOps(n int) []release.Operation {
	e.t.Helper()
	ops, err := e.svc.Store.ListOperations(context.Background(), "cache", 0, n)
	if err != nil {
		e.t.Fatal(err)
	}
	return ops
}

func TestResumeFinishesAnInterruptedUpgrade(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	must[*Result](t)(e.upgrade("image.tag=7.2"))
	// Stopped mid-replace: the old container is gone, the new one not created yet.
	e.crash("upgrade", 2, release.StatusUpgrading)
	delete(e.rt.Workloads, "cache-redis")

	e.svc.Lifecycle.Resume(context.Background())

	if r := e.release(); r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release: %+v", r)
	}
	if e.image() != "redis:7.2" {
		t.Fatalf("workload not recreated from revision 2: %q", e.image())
	}
	ops := e.lastOps(2)
	if ops[0].Type != ResumeOperation || ops[0].Status != release.OperationSucceeded || ops[1].Status != release.OperationAborted ||
		ops[0].Actor != ServerActor {
		t.Fatalf("operations: %+v", ops)
	}
	if rev := must[release.Revision](t)(e.svc.Store.GetRevision(context.Background(), "cache", 2)); rev.Status != release.RevisionDeployed {
		t.Fatalf("revision: %+v", rev)
	}
	revs := must[[]release.Revision](t)(e.svc.Store.ListRevisions(context.Background(), "cache"))
	if len(revs) != 2 {
		t.Fatalf("resume recorded a new revision: %d", len(revs))
	}
}

func TestResumeBeforeTheRevisionWasRecorded(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	e.crash("upgrade", 2, release.StatusUpgrading)
	e.svc.Lifecycle.Resume(context.Background())
	if r := e.release(); r.Status != release.StatusDeployed || r.CurrentRevision != 1 {
		t.Fatalf("release: %+v", r)
	}
	if ops := e.lastOps(1); ops[0].Status != release.OperationAborted {
		t.Fatalf("operation: %+v", ops[0])
	}
}

func TestResumeAbortsOtherOperationsAndUninstallsAgain(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	e.crash("volume-backup", 1, release.StatusDeployed)
	e.svc.Lifecycle.Resume(context.Background())
	if ops := e.lastOps(1); ops[0].Status != release.OperationAborted || e.release().Status != release.StatusDeployed {
		t.Fatalf("operation: %+v", ops[0])
	}

	// An uninstall resumes with its own options: --keep-history keeps the release, uninstalled.
	e.crashWith("uninstall", 1, release.StatusUninstalling, release.Options{KeepHistory: true})
	e.svc.Lifecycle.Resume(context.Background())
	if r := e.release(); r.Status != release.StatusUninstalled {
		t.Fatalf("release: %+v", r)
	}
	if _, ok := e.rt.Workloads["cache-redis"]; ok {
		t.Fatal("workload left after the resumed uninstall")
	}
	if len(e.rt.Volumes) == 0 {
		t.Fatal("the resumed uninstall deleted volumes it was not asked to")
	}

	// Without --keep-history the release goes.
	must[*Result](t)(e.install())
	e.crashWith("uninstall", 2, release.StatusUninstalling, release.Options{})
	e.svc.Lifecycle.Resume(context.Background())
	if _, err := e.svc.Store.GetRelease(context.Background(), "cache"); err == nil {
		t.Fatal("release kept by an uninstall without --keep-history")
	}
}

func TestOperationsRecordTheirUser(t *testing.T) {
	e := newEnv(t)
	ctx := WithActor(context.Background(), "alice")
	must[*Result](t)(e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts()}, nil))
	if op := e.lastOps(1)[0]; op.Actor != "alice" {
		t.Fatalf("actor: %+v", op)
	}
}
