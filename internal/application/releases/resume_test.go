package releases

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
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
	if err := e.svc.c.store.SaveRelease(ctx, rel); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.c.store.GetRevision(ctx, "cache", revision); err == nil {
		if err := e.svc.c.store.UpdateRevisionStatus(ctx, "cache", revision, release.RevisionPending, "Upgrade complete"); err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.svc.c.store.BeginOperation(ctx, release.Operation{Release: "cache", Type: op, Revision: revision,
		Status: release.OperationRunning, PID: 1 << 22, Hostname: e.svc.c.hostname, Options: opts, StartedAt: e.svc.c.now()}, nil); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) lastOps(n int) []release.Operation {
	e.t.Helper()
	ops, err := e.svc.c.store.ListOperations(context.Background(), "cache", 0, n)
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
	delete(e.rt.Workloads, e.rt.Name("cache-redis"))

	<-e.svc.Lifecycle.Resume(context.Background())

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
	if rev := must[release.Revision](t)(e.svc.c.store.GetRevision(context.Background(), "cache", 2)); rev.Status != release.RevisionDeployed {
		t.Fatalf("revision: %+v", rev)
	}
	revs := must[[]release.Revision](t)(e.svc.c.store.ListRevisions(context.Background(), "cache"))
	if len(revs) != 2 {
		t.Fatalf("resume recorded a new revision: %d", len(revs))
	}
}

func TestResumeBeforeTheRevisionWasRecorded(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	e.crash("upgrade", 2, release.StatusUpgrading)
	<-e.svc.Lifecycle.Resume(context.Background())
	if r := e.release(); r.Status != release.StatusDeployed || r.CurrentRevision != 1 {
		t.Fatalf("release: %+v", r)
	}
	if ops := e.lastOps(1); ops[0].Status != release.OperationAborted {
		t.Fatalf("operation: %+v", ops[0])
	}
}

// A failed release whose upgrade stopped before its revision was recorded stays failed: its status
// was not changed yet (it is written with the revision), and the reconciler must not take it over.
func TestResumeKeepsAFailedRelease(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	e.crash("upgrade", 2, release.StatusFailed)
	<-e.svc.Lifecycle.Resume(context.Background())
	if r := e.release(); r.Status != release.StatusFailed {
		t.Fatalf("release: %+v", r)
	}
}

// An uninstall stopped after it removed its release is closed when karga-server starts: a new
// install of the name is not taken for interrupted.
func TestResumeClosesAnUninstallWhoseReleaseIsGone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install())
	e.crash("uninstall", 1, release.StatusUninstalling)
	if err := e.svc.c.store.DeleteRelease(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	<-e.svc.Lifecycle.Resume(ctx)
	if ops := e.lastOps(1); ops[0].Type != "uninstall" || ops[0].Status != release.OperationSucceeded {
		t.Fatalf("operation: %+v", ops[0])
	}
	if _, err := e.install(); err != nil {
		t.Fatalf("install after: %v", err)
	}
}

// A resumed deployment prunes as its request asked (--history-max), not by the default.
func TestResumeKeepsHistoryMax(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	must[*Result](t)(e.upgrade("image.tag=7.1"))
	must[*Result](t)(e.upgrade("image.tag=7.2"))
	keep := 1
	e.crashWith("upgrade", 3, release.StatusUpgrading, release.Options{HistoryMax: &keep})
	<-e.svc.Lifecycle.Resume(context.Background())
	// The default (10) would keep all 3; 1 prunes to the current one and what is still in use.
	if revs := must[[]release.Revision](t)(e.svc.c.store.ListRevisions(context.Background(), "cache")); len(revs) == 3 {
		t.Fatalf("kept all %d revisions: the request's --history-max was not used", len(revs))
	}
}

func TestResumeAbortsOtherOperationsAndUninstallsAgain(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	e.crash("volume-backup", 1, release.StatusDeployed)
	<-e.svc.Lifecycle.Resume(context.Background())
	if ops := e.lastOps(1); ops[0].Status != release.OperationAborted || e.release().Status != release.StatusDeployed {
		t.Fatalf("operation: %+v", ops[0])
	}

	// An uninstall resumes with its own options: --keep-history keeps the release, uninstalled.
	e.crashWith("uninstall", 1, release.StatusUninstalling, release.Options{KeepHistory: true})
	<-e.svc.Lifecycle.Resume(context.Background())
	if r := e.release(); r.Status != release.StatusUninstalled {
		t.Fatalf("release: %+v", r)
	}
	if _, ok := e.rt.Lookup("cache-redis"); ok {
		t.Fatal("workload left after the resumed uninstall")
	}
	if len(e.rt.Volumes) == 0 {
		t.Fatal("the resumed uninstall deleted volumes it was not asked to")
	}

	// Without --keep-history the release goes.
	must[*Result](t)(e.install())
	e.crashWith("uninstall", 2, release.StatusUninstalling, release.Options{})
	<-e.svc.Lifecycle.Resume(context.Background())
	if _, err := e.svc.c.store.GetRelease(context.Background(), "cache"); err == nil {
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

// Resume holds the releases it resumes from the start, resumes them in the background, and tries
// again while the runtime is unavailable (karga-server started before Docker).
func TestResumeWaitsForTheRuntime(t *testing.T) {
	e := newEnv(t)
	must[*Result](t)(e.install())
	must[*Result](t)(e.upgrade("image.tag=7.2"))
	e.crash("upgrade", 2, release.StatusUpgrading)
	delete(e.rt.Workloads, e.rt.Name("cache-redis"))
	e.rt.Fail = map[string]error{"Info:": fmt.Errorf("docker is starting: %w", ports.ErrUnavailable)}
	resumeRetry = 20 * time.Millisecond
	t.Cleanup(func() { resumeRetry = 5 * time.Second })

	done := e.svc.Lifecycle.Resume(context.Background())
	// The release is held: a command waits for its resume instead of meeting it interrupted.
	if _, err := e.svc.c.locks.tryLock("cache"); !errors.Is(err, ErrLocked) {
		t.Fatalf("the release was not held while it waits to be resumed: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // a few failed tries
	e.rt.SetFail(nil)                  // Docker is up
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the resume did not finish once the runtime was back")
	}
	if r := e.release(); r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release: %+v", r)
	}
}

// An uninstall that cannot remove a container keeps the release's files, which the container
// mounts, and can be run again; one that finishes gives back the release's edge addresses (SA-05,
// SA-14).
func TestUninstallRemovesFilesLast(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must[*Result](t)(e.install("service.enabled=true"))
	edge := filepath.Join(e.svc.c.home, "edge.yaml")
	if data, _ := os.ReadFile(edge); !strings.Contains(string(data), "cache") {
		t.Fatalf("no edge addresses for cache: %s", data)
	}
	e.rt.Fail["DeleteWorkload:cache-redis"] = errors.New("device or resource busy")
	if _, err := e.svc.Lifecycle.Uninstall(ctx, UninstallRequest{Name: "cache"}); err == nil {
		t.Fatal("uninstall succeeded")
	}
	if _, err := os.Stat(filepath.Join(e.svc.c.home, "releases", "cache")); err != nil {
		t.Fatalf("the release's files went though its container stays: %v", err)
	}
	delete(e.rt.Fail, "DeleteWorkload:cache-redis")
	if _, err := e.svc.Lifecycle.Uninstall(ctx, UninstallRequest{Name: "cache", Force: true}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(edge); strings.Contains(string(data), "cache") {
		t.Fatalf("edge addresses kept after uninstall: %s", data)
	}
}
