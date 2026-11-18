//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

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
		return releases.Values{Set: []string{"image.tag=" + tag}}
	}

	// Install an older Redis, then upgrade forward (downgrading would hit RDB format changes).
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("7.2")}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("7.2")}, nil)
	if err != nil || !res.NoOp {
		t.Fatalf("second install should be a no-op: %+v %v", res, err)
	}
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("7.4")}, nil); err != nil {
		t.Fatal(err)
	}
	if st := workload(t, rt, name+"-redis"); st.Health != ports.HealthHealthy || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("after upgrade: %+v", st)
	}
	var out strings.Builder
	if err := s.Logs(ctx, releases.LogsRequest{Release: name, Tail: 50}, &out); err != nil || !strings.Contains(out.String(), "Ready to accept connections") {
		t.Fatalf("logs: %v\n%s", err, out.String())
	}

	// A missing image fails before the running container is touched.
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "../testdata/redis", Values: values("0.0.0-missing")}, nil); err == nil {
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
	res, err = s.Rollback(ctx, releases.RollbackRequest{CommonOptions: opts(), Name: name, Revision: 2}, nil)
	if err != nil || res.Revision != 4 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	rep, err := s.Status(ctx, name)
	if err != nil || rep.Release.Status != release.StatusDeployed {
		t.Fatalf("status: %+v %v", rep, err)
	}

	if _, err := s.Uninstall(ctx, releases.UninstallRequest{Name: name}); err != nil {
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

	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: pkgPath}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: pkgPath,
		Values: releases.Values{Set: []string{"healthy=false"}}}, nil)
	if err == nil {
		t.Fatal("unhealthy upgrade should fail")
	}
	hist, err := s.History(ctx, name)
	if err != nil || len(hist) != 2 || hist[0].Status != release.RevisionDeployed || hist[1].Status != release.RevisionFailed {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if r := currentRelease(t, s, name); r.CurrentRevision != 1 {
		t.Fatalf("current revision: %+v", r)
	}
	if st := workload(t, rt, name+"-probe"); st.State != ports.StateExited {
		t.Fatalf("failed container should be stopped and kept: %+v", st)
	}

	res, err := s.Rollback(ctx, releases.RollbackRequest{CommonOptions: common, Name: name, Revision: 1}, nil)
	if err != nil || res.Revision != 3 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if st := workload(t, rt, name+"-probe"); st.State != ports.StateRunning || st.Health != ports.HealthHealthy {
		t.Fatalf("after rollback: %+v", st)
	}
}
