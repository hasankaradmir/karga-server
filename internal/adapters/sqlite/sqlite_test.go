package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hasankaradmir/karga-sdk/release"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "karga.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenModeAndIdempotentMigrations(t *testing.T) {
	s, path := open(t)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("db mode: %v %v", info.Mode(), err)
	}
	_ = s.Close()
	again, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = again.Close()
}

func TestReleaseRevisionResourceCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	if _, err := s.GetRelease(ctx, "web"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing release: %v", err)
	}
	rel := release.Release{Name: "web", PackageName: "redis", PackageVersion: "1.0.0", Status: release.StatusDeploying, CreatedAt: now, UpdatedAt: now}
	must(t, s.SaveRelease(ctx, rel))
	rel.CurrentRevision, rel.Status = 1, release.StatusDeployed
	must(t, s.SaveRelease(ctx, rel))
	got, err := s.GetRelease(ctx, "web")
	must(t, err)
	if !reflect.DeepEqual(got, rel) {
		t.Fatalf("release round trip:\n got %+v\nwant %+v", got, rel)
	}

	rev := release.Revision{
		Release: "web", Number: 1, PackageName: "redis", PackageVersion: "1.0.0", RuntimeSupported: []string{"docker"},
		PackageDigest: "sha256:p", ValuesDigest: "sha256:v", ManifestDigest: "sha256:m",
		Values:    map[string]any{"port": 6379, "big": 1000000, "ratio": 0.5, "list": []any{1, "a"}},
		ValuesAll: map[string]any{"image": map[string]any{"tag": "7.4"}},
		Manifest:  json.RawMessage(`[{"kind":"Volume"}]`),
		Status:    release.RevisionPending, Description: "Install", CreatedAt: now,
	}
	must(t, s.CreateRevision(ctx, rev))
	must(t, s.UpdateRevisionStatus(ctx, "web", 1, release.RevisionDeployed, "Install complete"))
	gotRev, err := s.GetRevision(ctx, "web", 1)
	must(t, err)
	rev.Status, rev.Description = release.RevisionDeployed, "Install complete"
	if !reflect.DeepEqual(gotRev, rev) {
		t.Fatalf("revision round trip:\n got %#v\nwant %#v", gotRev, rev)
	}
	if err := s.UpdateRevisionStatus(ctx, "web", 9, release.RevisionFailed, ""); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("update missing revision: %v", err)
	}

	res := release.AppliedResource{Kind: "Workload", Name: "redis", Revision: 1, RuntimeName: "web-redis", Spec: json.RawMessage(`{"image":"redis"}`), Digest: "sha256:x"}
	must(t, s.SaveResource(ctx, "web", res))
	res.Revision = 2
	must(t, s.SaveResource(ctx, "web", res))
	list, err := s.ListResources(ctx, "web")
	must(t, err)
	if len(list) != 1 || !reflect.DeepEqual(list[0], res) {
		t.Fatalf("resources: %+v", list)
	}

	// Deleting a release cascades to revisions and resources.
	must(t, s.DeleteRelease(ctx, "web"))
	if revs, _ := s.ListRevisions(ctx, "web"); len(revs) != 0 {
		t.Fatalf("revisions left: %v", revs)
	}
	if rs, _ := s.ListResources(ctx, "web"); len(rs) != 0 {
		t.Fatalf("resources left: %v", rs)
	}
}

func TestOperations(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	op := release.Operation{Release: "web", Type: "install", Revision: 1, PID: 42, Hostname: "h", StartedAt: time.Now()}
	id, err := s.BeginOperation(ctx, op, nil)
	must(t, err)
	must(t, s.SaveOperationSteps(ctx, id, []release.Step{{Kind: "Workload", Name: "redis", Action: "create", Status: "done"}}))

	running, err := s.RunningOperation(ctx, "web")
	must(t, err)
	if running.ID != id || running.PID != 42 || len(running.Steps) != 1 {
		t.Fatalf("running: %+v", running)
	}

	locked := errors.New("locked")
	if _, err := s.BeginOperation(ctx, op, func(release.Operation) error { return locked }); !errors.Is(err, locked) {
		t.Fatalf("second begin: %v", err)
	}
	// Accepting the running operation marks it aborted.
	id2, err := s.BeginOperation(ctx, op, func(release.Operation) error { return nil })
	must(t, err)
	running, err = s.RunningOperation(ctx, "web")
	must(t, err)
	if running.ID != id2 {
		t.Fatalf("running after force: %+v", running)
	}

	running.Status = release.OperationSucceeded
	must(t, s.FinishOperation(ctx, running))
	if _, err := s.RunningOperation(ctx, "web"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("after finish: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackupAndSchemaGuards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "karga.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.SaveRelease(ctx, release.Release{Name: "web", PackageName: "p", PackageVersion: "1", Status: release.StatusDeployed, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dir, "copy.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, bak); err == nil {
		t.Fatal("backup over an existing file must fail")
	}
	if info, err := os.Stat(bak); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode: %v %v", info, err)
	}
	c, err := Open(ctx, bak)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetRelease(ctx, "web"); err != nil {
		t.Fatalf("backup misses the release: %v", err)
	}
	_ = c.Close()

	// A database of a newer karga is refused.
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer karga") {
		t.Fatalf("want newer-schema error, got %v", err)
	}

	// An older schema is copied before migrating.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	real, err := fs.ReadDir(embedded, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	extra := fstest.MapFS{"migrations/zzz_extra.sql": {Data: []byte("CREATE TABLE extra (x TEXT);")}}
	for _, e := range real {
		data, err := fs.ReadFile(embedded, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		extra["migrations/"+e.Name()] = &fstest.MapFile{Data: data}
	}
	old := migrations
	migrations = extra
	t.Cleanup(func() { migrations = old })
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != len(real)+1 {
		t.Fatalf("schema after migrate: %d %v", v, err)
	}
	if info, err := os.Stat(path + ".bak-v" + strconv.Itoa(len(real))); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("no backup before migrating: %v %v", info, err)
	}
}

func TestConcurrentOpenMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "karga.db")
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			s, err := Open(context.Background(), path)
			if err == nil {
				_ = s.Close()
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
}
