package releases

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
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
	if !slices.Equal(res.Stopped, []string{"db", "app", "reader", "worker"}) || !slices.Contains(events, "db ready") {
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
