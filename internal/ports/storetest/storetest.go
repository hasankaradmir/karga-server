// Package storetest is the contract every ports.StateStore passes.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Run runs the contract against the stores open returns, a new empty one per call.
func Run(t *testing.T, open func(t *testing.T) ports.StateStore) {
	t.Run("ReleaseRevisionResource", func(t *testing.T) { releaseRevisionResource(t, open(t)) })
	t.Run("Operations", func(t *testing.T) { operations(t, open(t)) })
	t.Run("ListOperations", func(t *testing.T) { listOperations(t, open(t)) })
	t.Run("InTx", func(t *testing.T) { inTx(t, open(t)) })
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func releaseRevisionResource(t *testing.T, s ports.StateStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if _, err := s.GetRelease(ctx, "web"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing release: %v", err)
	}
	rel := release.Release{Name: "web", PackageName: "redis", PackageVersion: "1.0.0", Status: release.StatusDeploying, CreatedAt: now, UpdatedAt: now}
	must(t, s.SaveRelease(ctx, rel))
	rel.CurrentRevision, rel.Status, rel.Paused = 1, release.StatusDeployed, true
	must(t, s.SaveRelease(ctx, rel))
	got, err := s.GetRelease(ctx, "web")
	must(t, err)
	if !got.CreatedAt.Equal(rel.CreatedAt) || !got.UpdatedAt.Equal(rel.UpdatedAt) {
		t.Fatalf("times: %+v", got)
	}
	got.CreatedAt, got.UpdatedAt = rel.CreatedAt, rel.UpdatedAt
	if !reflect.DeepEqual(got, rel) {
		t.Fatalf("release round trip:\n got %+v\nwant %+v", got, rel)
	}
	if rels, err := s.ListReleases(ctx); err != nil || len(rels) != 1 || rels[0].Name != "web" {
		t.Fatalf("list: %+v %v", rels, err)
	}

	rev := release.Revision{
		Release: "web", Number: 1, PackageName: "redis", PackageVersion: "1.0.0", RuntimeSupported: []string{"docker"},
		PackageDigest: "sha256:p", ValuesDigest: "sha256:v", ManifestDigest: "sha256:m",
		Values:    map[string]any{"port": 6379, "big": 1000000, "ratio": 0.5, "list": []any{1, "a"}, "tag": "7.4", "when": "2026-01-02"},
		ValuesAll: map[string]any{"image": map[string]any{"tag": "7.4"}},
		Manifest:  json.RawMessage(`[{"kind":"Volume","metadata":{"name":"data"}}]`),
		Notes:     "hello",
		Status:    release.RevisionPending, Description: "Install", CreatedAt: now,
	}
	must(t, s.CreateRevision(ctx, rev))
	must(t, s.UpdateRevisionStatus(ctx, "web", 1, release.RevisionDeployed, "Install complete"))
	gotRev, err := s.GetRevision(ctx, "web", 1)
	must(t, err)
	rev.Status, rev.Description = release.RevisionDeployed, "Install complete"
	if !gotRev.CreatedAt.Equal(rev.CreatedAt) {
		t.Fatalf("revision time: %v", gotRev.CreatedAt)
	}
	gotRev.CreatedAt = rev.CreatedAt
	var gotManifest, wantManifest any
	_ = json.Unmarshal(gotRev.Manifest, &gotManifest)
	_ = json.Unmarshal(rev.Manifest, &wantManifest)
	if !reflect.DeepEqual(gotManifest, wantManifest) {
		t.Fatalf("manifest: %s", gotRev.Manifest)
	}
	gotRev.Manifest = rev.Manifest
	if !reflect.DeepEqual(gotRev, rev) {
		t.Fatalf("revision round trip:\n got %#v\nwant %#v", gotRev, rev)
	}
	if err := s.UpdateRevisionStatus(ctx, "web", 9, release.RevisionFailed, ""); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("update missing revision: %v", err)
	}
	rev2 := rev
	rev2.Number = 10
	must(t, s.CreateRevision(ctx, rev2))
	revs, err := s.ListRevisions(ctx, "web")
	if err != nil || len(revs) != 2 || revs[0].Number != 1 || revs[1].Number != 10 {
		t.Fatalf("revisions in order: %+v %v", revs, err)
	}
	must(t, s.DeleteRevision(ctx, "web", 10))
	if revs, _ := s.ListRevisions(ctx, "web"); len(revs) != 1 {
		t.Fatalf("after delete: %+v", revs)
	}

	res := release.AppliedResource{Kind: "Workload", Name: "redis", Revision: 1, RuntimeName: "web-redis", Spec: json.RawMessage(`{"image":"redis","replicas":2}`), Digest: "sha256:x"}
	must(t, s.SaveResource(ctx, "web", res))
	res.Revision = 2
	must(t, s.SaveResource(ctx, "web", res))
	must(t, s.SaveResource(ctx, "web", release.AppliedResource{Kind: "Network", Name: "net", Spec: json.RawMessage(`{}`)}))
	list, err := s.ListResources(ctx, "web")
	must(t, err)
	if len(list) != 2 || list[0].Kind != "Network" || list[1].Revision != 2 || list[1].Digest != "sha256:x" {
		t.Fatalf("resources: %+v", list)
	}
	var spec map[string]any
	if err := json.Unmarshal(list[1].Spec, &spec); err != nil || spec["image"] != "redis" || spec["replicas"] != float64(2) {
		t.Fatalf("spec: %s %v", list[1].Spec, err)
	}
	must(t, s.DeleteResource(ctx, "web", "Network", "net"))
	if list, _ := s.ListResources(ctx, "web"); len(list) != 1 {
		t.Fatalf("after delete: %+v", list)
	}

	// Deleting a release takes its revisions and resources with it.
	must(t, s.DeleteRelease(ctx, "web"))
	if _, err := s.GetRelease(ctx, "web"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("deleted release: %v", err)
	}
	if revs, _ := s.ListRevisions(ctx, "web"); len(revs) != 0 {
		t.Fatalf("revisions left: %v", revs)
	}
	if rs, _ := s.ListResources(ctx, "web"); len(rs) != 0 {
		t.Fatalf("resources left: %v", rs)
	}
}

func operations(t *testing.T, s ports.StateStore) {
	ctx := context.Background()
	op := release.Operation{Release: "web", Type: "install", Revision: 1, PID: 42, Hostname: "h", Actor: "alice", StartedAt: time.Now()}
	id, err := s.BeginOperation(ctx, op, nil)
	must(t, err)
	must(t, s.SaveOperationSteps(ctx, id, []release.Step{{Kind: "Workload", Name: "redis", Action: "create", Status: "done"}}))

	running, err := s.RunningOperation(ctx, "web")
	must(t, err)
	if running.ID != id || running.PID != 42 || running.Actor != "alice" || len(running.Steps) != 1 {
		t.Fatalf("running: %+v", running)
	}

	locked := errors.New("locked")
	if _, err := s.BeginOperation(ctx, op, func(release.Operation) error { return locked }); !errors.Is(err, locked) {
		t.Fatalf("second begin: %v", err)
	}
	// Accepting the running operation marks it aborted.
	id2, err := s.BeginOperation(ctx, op, func(release.Operation) error { return nil })
	must(t, err)
	if id2 <= id {
		t.Fatalf("ids must grow: %d then %d", id, id2)
	}
	running, err = s.RunningOperation(ctx, "web")
	must(t, err)
	if running.ID != id2 {
		t.Fatalf("running after force: %+v", running)
	}
	ops, err := s.ListOperations(ctx, "web", 0, 10)
	must(t, err)
	if len(ops) != 2 || ops[1].Status != release.OperationAborted {
		t.Fatalf("aborted: %+v", ops)
	}

	running.Status, running.Error = release.OperationFailed, "boom"
	must(t, s.FinishOperation(ctx, running))
	if _, err := s.RunningOperation(ctx, "web"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("after finish: %v", err)
	}
	ops, _ = s.ListOperations(ctx, "web", 0, 10)
	if ops[0].Status != release.OperationFailed || ops[0].Error != "boom" || ops[0].FinishedAt.IsZero() {
		t.Fatalf("finished: %+v", ops[0])
	}
	// Operations outlive their release, like an audit log.
	must(t, s.SaveRelease(ctx, release.Release{Name: "web"}))
	must(t, s.DeleteRelease(ctx, "web"))
	if ops, _ := s.ListOperations(ctx, "web", 0, 10); len(ops) != 2 {
		t.Fatalf("operations of a deleted release: %+v", ops)
	}
}

func listOperations(t *testing.T, s ports.StateStore) {
	ctx := context.Background()
	var ids []int64
	for i, rel := range []string{"a", "b", "a", "c", "b"} {
		id, err := s.BeginOperation(ctx, release.Operation{Release: rel, Type: "install", Revision: i}, func(release.Operation) error { return nil })
		must(t, err)
		ids = append(ids, id)
	}
	all, err := s.ListOperations(ctx, "", 0, 0)
	must(t, err)
	if len(all) != 5 || all[0].ID != ids[4] || all[4].ID != ids[0] {
		t.Fatalf("all, newest first: %+v", all)
	}
	a, _ := s.ListOperations(ctx, "a", 0, 0)
	if len(a) != 2 || a[0].ID != ids[2] {
		t.Fatalf("release a: %+v", a)
	}
	after, _ := s.ListOperations(ctx, "", ids[2], 0)
	if len(after) != 2 || after[1].ID != ids[3] {
		t.Fatalf("after %d: %+v", ids[2], after)
	}
	if limited, _ := s.ListOperations(ctx, "", 0, 2); len(limited) != 2 || limited[0].ID != ids[4] {
		t.Fatalf("limit: %+v", limited)
	}
}

func inTx(t *testing.T, s ports.StateStore) {
	ctx := context.Background()
	rel := release.Release{Name: "web", Status: release.StatusDeployed}
	boom := errors.New("boom")
	err := s.InTx(ctx, func(st ports.StateStore) error {
		must(t, st.SaveRelease(ctx, rel))
		must(t, st.CreateRevision(ctx, release.Revision{Release: "web", Number: 1}))
		if _, err := st.GetRelease(ctx, "web"); err != nil {
			t.Fatalf("a transaction sees its own writes: %v", err)
		}
		if revs, _ := st.ListRevisions(ctx, "web"); len(revs) != 1 {
			t.Fatalf("a transaction lists its own writes: %+v", revs)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx: %v", err)
	}
	if _, err := s.GetRelease(ctx, "web"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("a failed transaction left its release: %v", err)
	}
	if revs, _ := s.ListRevisions(ctx, "web"); len(revs) != 0 {
		t.Fatalf("a failed transaction left its revision: %+v", revs)
	}
	must(t, s.InTx(ctx, func(st ports.StateStore) error {
		if err := st.SaveRelease(ctx, rel); err != nil {
			return err
		}
		return st.SaveResource(ctx, "web", release.AppliedResource{Kind: "Network", Name: "n", Spec: json.RawMessage(`{}`)})
	}))
	if _, err := s.GetRelease(ctx, "web"); err != nil {
		t.Fatalf("committed release missing: %v", err)
	}
	// Removing inside a transaction hides what it removed until it commits.
	must(t, s.InTx(ctx, func(st ports.StateStore) error {
		must(t, st.DeleteResource(ctx, "web", "Network", "n"))
		if rs, _ := st.ListResources(ctx, "web"); len(rs) != 0 {
			t.Fatalf("deleted resource still listed in the transaction: %+v", rs)
		}
		return nil
	}))
	if rs, _ := s.ListResources(ctx, "web"); len(rs) != 0 {
		t.Fatalf("deleted resource left: %+v", rs)
	}
}
