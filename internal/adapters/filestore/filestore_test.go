package filestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
	"github.com/hasankaradmir/karga-server/internal/ports/storetest"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) ports.StateStore { return open(t) })
}

func TestFilesAreYAML(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SaveRelease(ctx, release.Release{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginOperation(ctx, release.Operation{Release: "web", Type: "install"}, nil); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(p, ".yaml") {
			t.Errorf("%s is not a YAML file", p)
		}
		info, _ := d.Info()
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", p, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "state", "releases", "web", "release.yaml"))
	if !strings.Contains(string(data), "name: web") {
		t.Fatalf("release.yaml:\n%s", data)
	}
}

func TestOpenGuards(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "karga.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err == nil || !strings.Contains(err.Error(), "earlier karga build") {
		t.Fatalf("old database: %v", err)
	}
	home = t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state", "version.yaml"), []byte("version: 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err == nil || !strings.Contains(err.Error(), "newer karga-server") {
		t.Fatalf("newer format: %v", err)
	}
}

func TestOperationsArePruned(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for range keepOperations + 5 {
		id, err := s.BeginOperation(ctx, release.Operation{Release: "web", Type: "reconcile"}, func(release.Operation) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishOperation(ctx, release.Operation{ID: id, Release: "web", Status: release.OperationSucceeded}); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := s.ListOperations(ctx, "web", 0, 1000)
	if err != nil || len(ops) != keepOperations || ops[0].ID != keepOperations+5 {
		t.Fatalf("kept %d operations, newest %d: %v", len(ops), ops[0].ID, err)
	}
}

// A transaction that never reached its release file leaves files nobody refers to; the release
// stays as it was.
func TestInterruptedTransaction(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.SaveRelease(ctx, release.Release{Name: "web", CurrentRevision: 1, Status: release.StatusDeployed}); err != nil {
		t.Fatal(err)
	}
	tx := &Store{root: s.root, mu: s.mu, tx: &overlay{files: map[string][]byte{}}}
	if err := tx.CreateRevision(ctx, release.Revision{Release: "web", Number: 2}); err != nil {
		t.Fatal(err)
	}
	if err := tx.SaveRelease(ctx, release.Release{Name: "web", CurrentRevision: 2, Status: release.StatusUpgrading}); err != nil {
		t.Fatal(err)
	}
	// Apply everything but the release file, as a crash would.
	for _, n := range tx.tx.order {
		if !isReleaseFile(n) {
			if err := s.commit(n, tx.tx.files[n]); err != nil {
				t.Fatal(err)
			}
		}
	}
	rel, err := s.GetRelease(ctx, "web")
	if err != nil || rel.CurrentRevision != 1 || rel.Status != release.StatusDeployed {
		t.Fatalf("release after an interrupted transaction: %+v %v", rel, err)
	}
	// A new release interrupted the same way is not listed.
	tx2 := &Store{root: s.root, mu: s.mu, tx: &overlay{files: map[string][]byte{}}}
	if err := tx2.CreateRevision(ctx, release.Revision{Release: "new", Number: 1}); err != nil {
		t.Fatal(err)
	}
	for _, n := range tx2.tx.order {
		if err := s.commit(n, tx2.tx.files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if rels, _ := s.ListReleases(ctx); len(rels) != 1 {
		t.Fatalf("releases: %+v", rels)
	}
}

// Stores opened on the same home share their writes' order: no operation is lost or numbered twice.
func TestStoresOnOneHome(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	done := make(chan []int64)
	for range 4 {
		go func() {
			s, err := Open(home)
			if err != nil {
				t.Error(err)
			}
			var ids []int64
			for range 10 {
				id, err := s.BeginOperation(ctx, release.Operation{Release: "web", Type: "x"}, func(release.Operation) error { return nil })
				if err != nil {
					t.Error(err)
				}
				ids = append(ids, id)
			}
			done <- ids
		}()
	}
	seen := map[int64]bool{}
	for range 4 {
		for _, id := range <-done {
			if seen[id] {
				t.Fatalf("operation id %d given twice", id)
			}
			seen[id] = true
		}
	}
	s, _ := Open(home)
	if ops, _ := s.ListOperations(ctx, "web", 0, 1000); len(ops) != 40 {
		t.Fatalf("%d operations kept, want 40", len(ops))
	}
}

// Temporary files a crash left are removed when the state is opened again.
func TestOpenRemovesTemporaryFiles(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	left := filepath.Join(home, "state", "releases", ".release.yaml.tmp-123")
	if err := os.WriteFile(left, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}
}

// A release whose files cannot be read stops only itself: the others are listed, and their
// operations recorded (D-04).
func TestUnreadableReleaseStopsOnlyItself(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, n := range []string{"aaa", "bbb", "ccc"} {
		if err := s.SaveRelease(ctx, release.Release{Name: n, CurrentRevision: 1, Status: release.StatusDeployed}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginOperation(ctx, release.Operation{Release: n, Type: "install"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(home, "state", name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("releases/aaa/release.yaml", "id: [\nbad: {\n")        // not YAML
	write("releases/ccc/release.yaml", "name: ccc\npackage: re") // cut short: parses, fields empty
	write("operations/aaa.yaml", "id: [\nbad: {\n")

	rels, err := s.ListReleases(ctx)
	if err != nil || len(rels) != 3 || rels[0].Status != release.StatusUnreadable || rels[1].Status != release.StatusDeployed ||
		rels[2].Status != release.StatusUnreadable {
		t.Fatalf("list: %+v %v", rels, err)
	}
	if _, err := s.GetRelease(ctx, "aaa"); err == nil || !strings.Contains(err.Error(), "aaa/release.yaml") {
		t.Fatalf("aaa: %v", err)
	}
	if _, err := s.GetRelease(ctx, "ccc"); err == nil || !strings.Contains(err.Error(), "ccc/release.yaml is incomplete") {
		t.Fatalf("ccc: %v", err)
	}
	// bbb's operation goes on as if aaa's file were fine.
	op, err := s.RunningOperation(ctx, "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveOperationSteps(ctx, "bbb", op.ID, []release.Step{{Kind: "Workload", Name: "web", Action: "create"}}); err != nil {
		t.Fatal(err)
	}
	op.Status = release.OperationSucceeded
	if err := s.FinishOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListOperations(ctx, "", 0, 0)
	if err != nil || len(all) != 2 || all[1].Release != "bbb" || all[1].Status != release.OperationSucceeded {
		t.Fatalf("every release's operations: %+v %v", all, err)
	}
	if _, err := s.ListOperations(ctx, "aaa", 0, 0); err == nil || !strings.Contains(err.Error(), "operations/aaa.yaml") {
		t.Fatalf("aaa's operations: %v", err)
	}
}
