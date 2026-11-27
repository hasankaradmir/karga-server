package releases

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/packages"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

const volPkg = "testdata/volapp"

func installVolApp(t *testing.T, sets ...string) *env {
	t.Helper()
	e := newEnv(t)
	if _, err := e.svc.Lifecycle.Install(context.Background(), InstallRequest{CommonOptions: common(), Name: "vol", Package: volPkg,
		Values: Values{Sets: packages.Sets(sets...)}}, nil); err != nil {
		t.Fatal(err)
	}
	e.rt.Calls = nil
	return e
}

// calls returns the fake runtime's calls of op ("StopWorkload:vol-db" -> "db").
func calls(e *env, op string) []string {
	var out []string
	for _, c := range e.rt.Calls {
		if n, ok := strings.CutPrefix(c, op+":vol-"); ok {
			out = append(out, n)
		}
	}
	return out
}

// A backup stops what writes to the volume and what depends on it, dependents first, and starts
// them again in order; a reader of the volume and an unrelated workload keep running.
func TestVolumeBackupStopsDependents(t *testing.T) {
	e := installVolApp(t)
	var out bytes.Buffer
	res, err := e.svc.Volumes.Backup(context.Background(), VolumeBackupRequest{Release: "vol", Volume: "data"}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(e, "StopWorkload"); !slices.Equal(got, []string{"worker", "app", "db"}) {
		t.Fatalf("stopped %v", got)
	}
	if got := calls(e, "StartWorkload"); !slices.Equal(got, []string{"db", "app", "worker"}) {
		t.Fatalf("started %v", got)
	}
	if !slices.Equal(res.Stopped, []string{"db", "app", "worker"}) || out.Len() == 0 {
		t.Fatalf("result %+v, %d bytes", res, out.Len())
	}
	ops, _ := e.svc.c.store.ListOperations(context.Background(), "vol", 0, 10)
	if last := ops[0]; last.Type != "volume-backup" || last.Downtime != res.Downtime || res.Downtime <= 0 {
		t.Fatalf("operation %+v, result downtime %s", last, res.Downtime)
	}
}

func TestVolumeLiveBackupStopsNothing(t *testing.T) {
	e := installVolApp(t)
	res, err := e.svc.Volumes.Backup(context.Background(), VolumeBackupRequest{Release: "vol", Volume: "data", Live: true}, &bytes.Buffer{}, nil)
	if err != nil || len(calls(e, "StopWorkload")) != 0 || len(res.Stopped) != 0 || res.Downtime != 0 {
		t.Fatalf("live backup: %+v %v %v", res, calls(e, "StopWorkload"), err)
	}
}

// A restore stops the reader too: it must not see the files change.
func TestVolumeRestoreStopsReaders(t *testing.T) {
	e := installVolApp(t)
	var backup bytes.Buffer
	if _, err := e.svc.Volumes.Backup(context.Background(), VolumeBackupRequest{Release: "vol", Volume: "data", Live: true}, &backup, nil); err != nil {
		t.Fatal(err)
	}
	var events []string
	res, err := e.svc.Volumes.Restore(context.Background(), VolumeRestoreRequest{Release: "vol", Volume: "data", Name: "b.tgz"}, &backup,
		func(ev deployment.Event) { events = append(events, ev.Name+" "+ev.Detail) })
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(e, "StopWorkload"); !slices.Equal(got, []string{"worker", "reader", "app", "db"}) {
		t.Fatalf("stopped %v", got)
	}
	if !slices.Equal(res.Stopped, []string{"db", "app", "reader", "worker"}) || !slices.Contains(events, "db start") {
		t.Fatalf("result %+v events %v", res, events)
	}
}

// When a workload does not get ready again, the backup is still complete: it is sent, and the error
// says so (ErrNotReady).
func TestVolumeBackupNotReady(t *testing.T) {
	e := installVolApp(t)
	e.rt.Health = ports.HealthUnhealthy
	var out bytes.Buffer
	res, err := e.svc.Volumes.Backup(context.Background(), VolumeBackupRequest{Release: "vol", Volume: "data"}, &out, nil)
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "volume data of vol copied, but workload db did not get ready") {
		t.Fatalf("error: %v", err)
	}
	if res == nil || out.Len() == 0 || len(calls(e, "StartWorkload")) != 3 {
		t.Fatalf("result %+v, %d bytes, started %v", res, out.Len(), calls(e, "StartWorkload"))
	}
}

// A client that goes away mid-copy (Ctrl-C) stops the copy, not the start of the workloads: they
// run again, ready, in order.
func TestVolumeBackupStoppedByTheClientStartsTheWorkloads(t *testing.T) {
	e := installVolApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.svc.Volumes.Backup(ctx, VolumeBackupRequest{Release: "vol", Volume: "data"}, &bytes.Buffer{}, func(ev deployment.Event) {
		if ev.Detail == "copying" {
			cancel()
		}
	})
	if errors.Is(err, ErrNotReady) {
		t.Fatalf("error: %v", err)
	}
	if got := calls(e, "StartWorkload"); !slices.Equal(got, []string{"db", "app", "worker"}) {
		t.Fatalf("started %v", got)
	}
}

// put writes files into the volume through db's container.
func put(t *testing.T, e *env, files ...string) {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f, Mode: 0o644, Size: int64(len(f))})
		_, _ = tw.Write([]byte(f))
	}
	_ = tw.Close()
	if err := e.rt.ImportPath(context.Background(), e.rt.Containers("vol", "db")[0], "/var/lib/db", &b); err != nil {
		t.Fatal(err)
	}
}

// volumeFiles lists the files in the volume.
func volumeFiles(e *env) []string {
	return slices.Sorted(maps.Keys(e.rt.VolumeFiles["vol-data"]))
}

func liveBackup(t *testing.T, e *env) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	if _, err := e.svc.Volumes.Backup(context.Background(), VolumeBackupRequest{Release: "vol", Volume: "data", Live: true}, &b, nil); err != nil {
		t.Fatal(err)
	}
	return &b
}

// A restore replaces the volume's files: a file written after the backup is gone. The files it had
// are its restore point, and undo-restore puts them back.
func TestVolumeRestoreReplacesAndUndoes(t *testing.T) {
	e := installVolApp(t)
	ctx := context.Background()
	put(t, e, "a")
	backup := liveBackup(t, e)
	put(t, e, "b")
	res, err := e.svc.Volumes.Restore(ctx, VolumeRestoreRequest{Release: "vol", Volume: "data", Name: "b.tgz"}, backup, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := volumeFiles(e); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("after restore: %v", got)
	}
	if res.RestorePoint != restorePointPath(e.svc.c.home, "vol", "data") {
		t.Fatalf("restore point %q", res.RestorePoint)
	}
	// The containers are the same, created again and running.
	if st, _ := e.rt.GetWorkload(ctx, e.rt.Containers("vol", "db")[0]); st.State != ports.StateRunning {
		t.Fatalf("db: %+v", st)
	}
	if left, _ := os.ReadDir(filepath.Join(e.svc.c.home, "restores")); len(left) != 0 {
		t.Fatalf("the upload is kept after the restore: %v", left)
	}
	undo, err := e.svc.Volumes.UndoRestore(ctx, "vol", "data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := volumeFiles(e); !slices.Equal(got, []string{"a", "b"}) || len(undo.Stopped) != 4 {
		t.Fatalf("after undo: %v %+v", got, undo)
	}
	if _, err := os.Stat(res.RestorePoint); err != nil {
		t.Fatalf("undo removed the restore point: %v", err)
	}
	if _, err := e.svc.Volumes.UndoRestore(ctx, "vol", "other", nil); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("undo without a restore point: %v", err)
	}
	// uninstall --delete-volumes takes the restore points with the volumes.
	if _, err := e.svc.Lifecycle.Uninstall(ctx, UninstallRequest{Name: "vol", DeleteVolumes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.RestorePoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore point after uninstall --delete-volumes: %v", err)
	}
}

func TestVolumeRestoreMergeKeepsOtherFiles(t *testing.T) {
	e := installVolApp(t)
	put(t, e, "a")
	backup := liveBackup(t, e)
	put(t, e, "b")
	res, err := e.svc.Volumes.Restore(context.Background(), VolumeRestoreRequest{Release: "vol", Volume: "data", Merge: true, NoRestorePoint: true}, backup, nil)
	if err != nil || !slices.Equal(volumeFiles(e), []string{"a", "b"}) || res.RestorePoint != "" || len(calls(e, "DeleteVolume")) != 0 {
		t.Fatalf("merge: %v %v %+v", volumeFiles(e), err, res)
	}
	if _, err := os.Stat(restorePointPath(e.svc.c.home, "vol", "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--no-restore-point kept one: %v", err)
	}
}

// Without room for the restore point nothing is touched.
func TestVolumeRestoreNoRoom(t *testing.T) {
	e := installVolApp(t)
	put(t, e, "a")
	backup := liveBackup(t, e)
	put(t, e, "b")
	e.svc.c.freeSpace = func(string) (uint64, error) { return 10, nil }
	_, err := e.svc.Volumes.Restore(context.Background(), VolumeRestoreRequest{Release: "vol", Volume: "data"}, backup, nil)
	if !errors.Is(err, ports.ErrInvalid) || !strings.Contains(err.Error(), "--no-restore-point") {
		t.Fatalf("error: %v", err)
	}
	if len(calls(e, "StopWorkload")) != 0 || !slices.Equal(volumeFiles(e), []string{"a", "b"}) {
		t.Fatalf("touched: stopped %v, files %v", calls(e, "StopWorkload"), volumeFiles(e))
	}
}

// A backup cut short is refused before anything changes: its gzip check fails at the end.
func TestVolumeRestoreRefusesABackupCutShort(t *testing.T) {
	e := installVolApp(t)
	put(t, e, "a")
	backup := liveBackup(t, e).Bytes()
	_, err := e.svc.Volumes.Restore(context.Background(), VolumeRestoreRequest{Release: "vol", Volume: "data", Name: "b.tgz"},
		bytes.NewReader(backup[:len(backup)-8]), nil)
	if !errors.Is(err, ports.ErrInvalid) || !strings.Contains(err.Error(), "b.tgz is cut short or damaged") {
		t.Fatalf("error: %v", err)
	}
	if len(calls(e, "StopWorkload")) != 0 || !slices.Equal(volumeFiles(e), []string{"a"}) {
		t.Fatalf("touched: stopped %v, files %v", calls(e, "StopWorkload"), volumeFiles(e))
	}
}

// A restore that fails writing the volume leaves its workloads stopped and the release paused: the
// volume may be empty or half written, and the reconciler must not start them on it.
func TestVolumeRestoreWriteFailureKeepsTheWorkloadsStopped(t *testing.T) {
	e := installVolApp(t)
	ctx := context.Background()
	put(t, e, "a")
	backup := liveBackup(t, e)
	e.rt.Fail = map[string]error{"ImportPath:vol-db": errors.New("no space left on device")}
	_, err := e.svc.Volumes.Restore(ctx, VolumeRestoreRequest{Release: "vol", Volume: "data", Name: "b.tgz"}, backup, nil)
	if err == nil || !strings.Contains(err.Error(), "no space left on device") ||
		!strings.Contains(err.Error(), "db, app, reader, worker stay stopped and vol is paused") ||
		!strings.Contains(err.Error(), "'karga volume undo-restore vol data'") {
		t.Fatalf("error: %v", err)
	}
	if got := calls(e, "StartWorkload"); len(got) != 0 {
		t.Fatalf("started %v", got)
	}
	if rel, _ := e.svc.c.store.GetRelease(ctx, "vol"); !rel.Paused {
		t.Fatalf("release not paused: %+v", rel)
	}
	ops, _ := e.svc.c.store.ListOperations(ctx, "vol", 0, 1)
	if ops[0].Type != "volume-restore" || ops[0].Status != release.OperationFailed {
		t.Fatalf("operation: %+v", ops[0])
	}
}

// A restore karga-server stopped in the middle is finished when it starts again, from the backup it
// kept, without taking the restore point again from a volume that may be half written.
func TestVolumeRestoreResumes(t *testing.T) {
	e := installVolApp(t)
	ctx := context.Background()
	put(t, e, "a")
	backup := liveBackup(t, e)
	from, err := e.svc.Volumes.keep(backup)
	if err != nil {
		t.Fatal(err)
	}
	point := restorePointPath(e.svc.c.home, "vol", "data")
	if err := os.MkdirAll(filepath.Dir(point), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(point, []byte("the one taken before the crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What a process stopped after the restore point and before the volume was written leaves.
	put(t, e, "half")
	op, err := e.svc.c.begin(ctx, "vol", "volume-restore", 1, false, release.Options{Restore: &release.RestoreOptions{Volume: "data", From: from, Name: "b.tgz"}})
	if err != nil {
		t.Fatal(err)
	}
	op.Steps = []release.Step{{Kind: resource.KindVolume, Name: "data", Action: restorePointStep, Status: deployment.StepDone}}
	if err := e.svc.c.store.SaveOperationSteps(ctx, op.Release, op.ID, op.Steps); err != nil {
		t.Fatal(err)
	}
	// The interrupted restore had stopped them all.
	for _, n := range []string{"worker", "reader", "app", "db"} {
		if err := e.rt.StopWorkload(ctx, e.rt.Containers("vol", n)[0], 0); err != nil {
			t.Fatal(err)
		}
	}
	e.rt.Calls = nil
	if err := e.svc.Lifecycle.resumeRelease(ctx, "vol"); err != nil {
		t.Fatal(err)
	}
	if got := volumeFiles(e); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("after resume: %v", got)
	}
	// The resume starts them again itself, in order, rather than leave them to the reconciler.
	if got := calls(e, "StartWorkload"); !slices.Equal(got, []string{"db", "app", "reader", "worker"}) {
		t.Fatalf("started %v", got)
	}
	if data, _ := os.ReadFile(point); string(data) != "the one taken before the crash" {
		t.Fatalf("the resume took the restore point again: %q", data)
	}
	if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the kept backup is still there: %v", err)
	}
	ops, _ := e.svc.c.store.ListOperations(ctx, "vol", 0, 10)
	if ops[0].Type != "volume-restore" || ops[0].Status != release.OperationSucceeded || ops[1].Status != release.OperationAborted {
		t.Fatalf("operations: %+v / %+v", ops[0], ops[1])
	}
}

// A resumed restore that fails before it begins (here: its free-space check) keeps the backup: the
// operation stays running, and the next start resumes it from that backup.
func TestVolumeRestoreResumeKeepsBackupOnEarlyError(t *testing.T) {
	e := installVolApp(t)
	ctx := context.Background()
	put(t, e, "a")
	from, err := e.svc.Volumes.keep(liveBackup(t, e))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.c.begin(ctx, "vol", "volume-restore", 1, false, release.Options{Restore: &release.RestoreOptions{Volume: "data", From: from, Name: "b.tgz"}}); err != nil {
		t.Fatal(err)
	}
	e.svc.c.freeSpace = func(string) (uint64, error) { return 0, errors.New("statfs: input/output error") }
	if err := e.svc.Lifecycle.resumeRelease(ctx, "vol"); err == nil {
		t.Fatal("resume did not fail")
	}
	if _, err := os.Stat(from); err != nil {
		t.Fatalf("the kept backup is gone: %v", err)
	}
	if op, err := e.svc.c.store.RunningOperation(ctx, "vol"); err != nil || op.Type != "volume-restore" {
		t.Fatalf("the restore is not left running: %+v %v", op, err)
	}

	e.svc.c.freeSpace = freeSpace
	if err := e.svc.Lifecycle.resumeRelease(ctx, "vol"); err != nil {
		t.Fatal(err)
	}
	if got := volumeFiles(e); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("after resume: %v", got)
	}
	if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the kept backup is still there: %v", err)
	}
}

// Without its backup an interrupted restore cannot be finished: the release is paused, so the
// reconciler does not start the workloads on a half-written volume.
func TestVolumeRestoreResumeWithoutBackup(t *testing.T) {
	e := installVolApp(t)
	ctx := context.Background()
	if _, err := e.svc.c.begin(ctx, "vol", "volume-restore", 1, false, release.Options{Restore: &release.RestoreOptions{Volume: "data", From: "/nonexistent"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Lifecycle.resumeRelease(ctx, "vol"); err != nil {
		t.Fatal(err)
	}
	if rel, _ := e.svc.c.store.GetRelease(ctx, "vol"); !rel.Paused {
		t.Fatalf("release not paused: %+v", rel)
	}
	ops, _ := e.svc.c.store.ListOperations(ctx, "vol", 0, 1)
	if ops[0].Status != release.OperationAborted || !strings.Contains(ops[0].Error, "undo-restore") {
		t.Fatalf("operation: %+v", ops[0])
	}
}
