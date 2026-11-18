//go:build integration

package integration

import (
	"bytes"
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// service returns a release service on real Docker and a fresh state directory. Everything the
// release created is removed when the test ends, volumes included.
func TestRedisReleaseLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-rel")
	s, rt := service(t, name)
	values := func(tag string) releases.Values {
		return releases.Values{Sets: packages.Sets([]string{"image.tag=" + tag}...)}
	}

	// Install an older Redis, then upgrade forward (downgrading would hit RDB format changes).
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("7.2")}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("7.2")}, nil)
	if err != nil || !res.NoOp {
		t.Fatalf("second install should be a no-op: %+v %v", res, err)
	}
	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("7.4")}, nil); err != nil {
		t.Fatal(err)
	}
	if st := workload(t, rt, name+"-redis"); st.Health != ports.HealthHealthy || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("after upgrade: %+v", st)
	}
	var out strings.Builder
	if err := s.Reads.Logs(ctx, releases.LogsRequest{Release: name, Tail: 50}, &out); err != nil || !strings.Contains(out.String(), "Ready to accept connections") {
		t.Fatalf("logs: %v\n%s", err, out.String())
	}

	// A missing image fails before the running container is touched.
	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("0.0.0-missing")}, nil); err == nil {
		t.Fatal("upgrade to a missing image should fail")
	}
	if st := workload(t, rt, name+"-redis"); st.State != ports.StateRunning || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("running workload was touched by a failed pull: %+v", st)
	}
	// Nothing that runs was changed, so the release stays deployed at revision 2 (D18).
	if r := currentRelease(t, s, name); r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed upgrade: %+v", r)
	}

	// Rolling back to the current revision is allowed (and changes nothing). Revision 1 would be a
	// Redis downgrade, which cannot read the newer data format.
	res, err = s.Lifecycle.Rollback(ctx, releases.RollbackRequest{CommonOptions: opts(), Name: name, Revision: 2}, nil)
	if err != nil || res.Revision != 4 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	rep, err := s.Reads.Status(ctx, name)
	if err != nil || rep.Release.Status != release.StatusDeployed {
		t.Fatalf("status: %+v %v", rep, err)
	}

	if _, err := s.Lifecycle.Uninstall(ctx, releases.UninstallRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if objs := managed(t, rt, name); len(objs) != 1 || objs[0].Kind != ports.ObjectVolume {
		t.Fatalf("after uninstall only the volume should remain: %+v", objs)
	}
}

// TestFailedUpgradeKeepsHistory is the M3 exit criterion: a failed upgrade leaves revision 1 deployed
// and current, the failing container stopped but present, and rollback brings back a healthy state.
func TestFailedUpgradeKeepsHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-probe")
	s, rt := service(t, name)
	pkgPath := "testdata/probe"
	common := opts()
	common.Timeout = 30 * time.Second

	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: pkgPath}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: pkgPath,
		Values: releases.Values{Sets: packages.Sets([]string{"healthy=false"}...)}}, nil)
	if err == nil {
		t.Fatal("unhealthy upgrade should fail")
	}
	hist, err := s.Reads.History(ctx, name)
	if err != nil || len(hist) != 2 || hist[0].Status != release.RevisionDeployed || hist[1].Status != release.RevisionFailed {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if r := currentRelease(t, s, name); r.CurrentRevision != 1 {
		t.Fatalf("current revision: %+v", r)
	}
	if st := workload(t, rt, name+"-probe"); st.State != ports.StateExited {
		t.Fatalf("failed container should be stopped and kept: %+v", st)
	}

	res, err := s.Lifecycle.Rollback(ctx, releases.RollbackRequest{CommonOptions: common, Name: name, Revision: 1}, nil)
	if err != nil || res.Revision != 3 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if st := workload(t, rt, name+"-probe"); st.State != ports.StateRunning || st.Health != ports.HealthHealthy {
		t.Fatalf("after rollback: %+v", st)
	}
}

// A variable from a Secret reaches the container, never the state, and a new value recreates it.
func TestEnvironmentFromSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-envsec")
	s, rt := service(t, name)
	printenv := func() string {
		t.Helper()
		var out bytes.Buffer
		if code, err := rt.Exec(ctx, name+"-web", ports.ExecOptions{Cmd: []string{"printenv", "SECRET_VALUE"}, Stdout: &out, Stderr: &out}); err != nil || code != 0 {
			t.Fatalf("printenv: %d %v %s", code, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	first := "first-" + name
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"envSecret=" + first, "strategy=recreate"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := printenv(); got != first {
		t.Fatalf("SECRET_VALUE = %q", got)
	}
	state := filepath.Join(sharedHome, "state", "releases", name)
	_ = filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && bytes.Contains(readFile(t, p), []byte(first)) {
			t.Errorf("%s holds the secret value", p)
		}
		return nil
	})
	second := "second-" + name
	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"envSecret=" + second, "strategy=recreate"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := printenv(); got != second {
		t.Fatalf("after upgrade SECRET_VALUE = %q", got)
	}
}
