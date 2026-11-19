package releases

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

var update = flag.Bool("update", false, "rewrite golden files")

// lifecycleStep is what one step of the scenario did, in a form that does not change from run to run.
type lifecycleStep struct {
	Step      string   `yaml:"step"`
	Error     string   `yaml:"error,omitempty"`
	Result    any      `yaml:"result,omitempty"`
	Release   string   `yaml:"release"`
	Revisions []string `yaml:"revisions,omitempty"`
	Ops       []string `yaml:"operations,omitempty"`
	Calls     []string `yaml:"runtimeCalls,omitempty"`
}

// TestLifecycleGolden pins what the release lifecycle does today, step by step: the result, the
// stored state and the runtime calls of install, a repeated install, upgrades, a failed upgrade,
// rollback, a repair, a resumed operation and uninstall. Restructuring the server must leave it
// unchanged. Run go test ./internal/application/releases -run TestLifecycleGolden -update after an
// intended change of behavior.
func TestLifecycleGolden(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var steps []lifecycleStep
	record := func(name string, res any, err error) {
		t.Helper()
		s := lifecycleStep{Step: name, Result: summarize(res), Calls: e.rt.Calls}
		if err != nil {
			s.Error = strings.SplitN(err.Error(), "\n", 2)[0]
		}
		e.rt.Calls = nil
		if rel, err := e.svc.c.store.GetRelease(ctx, "cache"); err == nil {
			s.Release = fmt.Sprintf("%s, revision %d", rel.Status, rel.CurrentRevision)
		} else {
			s.Release = "absent"
		}
		revs, _ := e.svc.c.store.ListRevisions(ctx, "cache")
		for _, r := range revs {
			s.Revisions = append(s.Revisions, fmt.Sprintf("%d %s", r.Number, r.Status))
		}
		ops, _ := e.svc.c.store.ListOperations(ctx, "cache", 0, 100)
		for _, o := range slices.Backward(ops) {
			s.Ops = append(s.Ops, fmt.Sprintf("%s %d %s", o.Type, o.Revision, o.Status))
		}
		steps = append(steps, s)
	}
	upgrade := func(sets ...string) (*Result, error) { return e.upgrade(sets...) }

	res, err := e.install()
	record("install", res, err)
	res, err = e.install()
	record("install again (same package and values)", res, err)
	res, err = upgrade("image.tag=7.4.1")
	record("upgrade image", res, err)
	res, err = upgrade("image.tag=7.4.1", "config.maxmemory=128mb")
	record("upgrade config", res, err)

	e.rt.Fail = map[string]error{"StartWorkload:cache-redis": errors.New("boom")}
	res, err = upgrade("image.tag=7.4.2", "config.maxmemory=128mb")
	record("upgrade that fails to start", res, err)
	e.rt.Fail = nil

	res, err = e.svc.Lifecycle.Rollback(ctx, RollbackRequest{CommonOptions: common(), Name: "cache", Revision: 2}, nil)
	record("rollback to 2", res, err)

	if err := e.rt.DeleteWorkload(ctx, e.rt.Name("cache-redis")); err != nil {
		t.Fatal(err)
	}
	e.rt.Calls = nil
	rep, err := e.svc.Lifecycle.Repair(ctx, "cache", false)
	record("repair a removed container", rep, err)

	// A karga-server that stopped in the middle of an upgrade to revision 5: the revision is recorded,
	// the operation still running.
	res, err = e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: CommonOptions{DryRun: true}, Name: "cache", Package: redisPkg, Values: e.opts("image.tag=7.4.3")}, nil)
	record("dry-run upgrade", res, err)
	if _, err := e.svc.c.store.BeginOperation(ctx, release.Operation{Release: "cache", Type: "upgrade", Revision: 5,
		PID: deadPID(t), Hostname: e.svc.c.hostname, StartedAt: time.Now()}, func(release.Operation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	<-e.svc.Lifecycle.Resume(ctx)
	record("resume an interrupted upgrade", nil, nil)

	res, err = e.svc.Lifecycle.Uninstall(ctx, UninstallRequest{Name: "cache"})
	record("uninstall", res, err)

	got, err := yaml.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "lifecycle.golden.yaml")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("lifecycle differs from %s (run with -update if intended)\n--- got:\n%s", golden, got)
	}
}

// summarize keeps the parts of a result that describe behavior, without times and digests.
func summarize(v any) any {
	switch r := v.(type) {
	case *Result:
		if r == nil {
			return nil
		}
		out := map[string]any{"status": r.Status, "revision": r.Revision, "from": r.From}
		if r.NoOp {
			out["noOp"] = true
		}
		if r.DryRun {
			out["dryRun"] = true
		}
		var plan []string
		for _, c := range r.Plan {
			line := c.Action + " " + c.Kind + "/" + c.Name
			if c.Strategy != "" {
				line += " (" + c.Strategy + ")"
			}
			for _, f := range c.Fields {
				line += " " + f.Path
			}
			plan = append(plan, line)
		}
		if plan != nil {
			out["plan"] = plan
		}
		if r.Warnings != nil {
			out["warnings"] = r.Warnings
		}
		return out
	case *ReconcileResult:
		if r == nil {
			return nil
		}
		out := map[string]any{}
		if r.Skipped != "" {
			out["skipped"] = r.Skipped
		}
		var actions []string
		for _, a := range r.Actions {
			actions = append(actions, fmt.Sprintf("%s %s/%s %s", a.Action, a.Kind, a.Name, a.Status))
		}
		if actions != nil {
			out["actions"] = actions
		}
		return out
	}
	return nil
}
