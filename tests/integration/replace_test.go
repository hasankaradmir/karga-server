//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// startDelay makes the server take 3s to listen: a recreate then has a real outage, a replace must
// hide it.
const startDelay = "startDelay=3"

// maxRaceFailures bounds requests that may fail while racing a replace switch-over (see below).
const maxRaceFailures = 2

func TestReplaceWithoutDowntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-web")
	s, rt := service(t, name)
	common := opts()
	common.Timeout = 30 * time.Second
	upgrade := func(sets ...string) (*releases.Result, error) {
		return s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg,
			Values: releases.Values{Set: append([]string{startDelay}, sets...)}}, nil)
	}

	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{startDelay}}}, nil); err != nil {
		t.Fatal(err)
	}
	pinger := startNetPinger(t, rt, name, "http://web:8080/")
	time.Sleep(time.Second)

	res, err := upgrade("content=v2")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Plan {
		if c.Kind == "Workload" && c.Strategy != "replace" {
			t.Fatalf("expected replace, got %+v", c)
		}
	}
	time.Sleep(time.Second)

	// A broken new version never takes traffic and the old one keeps serving.
	if _, err := upgrade("content=v3", "healthy=false"); err == nil {
		t.Fatal("unhealthy replace should fail")
	}
	time.Sleep(time.Second)

	got := pinger.result()
	// There is no outage window, but a request that resolved the old container's IP just before it
	// stopped can still fail: DNS-based switching cannot drain that race (karga's proxies can, on
	// Linux). Allow at most maxRaceFailures; the recreate control shows a real outage is far larger.
	t.Logf("replace: %s", got)
	if got.Failed > maxRaceFailures {
		t.Fatalf("%d failed requests during replace (max %d): %s", got.Failed, maxRaceFailures, got)
	}
	if got.Bodies["v1"] == 0 || got.Bodies["v2"] == 0 || got.Bodies["v3"] != 0 {
		t.Fatalf("unexpected content: %s", got)
	}

	st := workload(t, rt, name+"-web")
	if st.State != ports.StateRunning || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("canonical container: %+v", st)
	}
	r := currentRelease(t, s, name)
	if r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed replace: %+v", r)
	}
	hist, err := s.Reads.History(ctx, name)
	if err != nil || len(hist) != 3 || hist[2].Status != release.RevisionFailed {
		t.Fatalf("history: %+v %v", hist, err)
	}
}

// TestRecreateHasDowntime is the control for TestReplaceWithoutDowntime: the same upgrade with
// recreate must be seen by the pinger, otherwise the pinger proves nothing.
func TestRecreateHasDowntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-web-rc")
	s, rt := service(t, name)
	common := opts()
	common.Timeout = 30 * time.Second
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{startDelay, "strategy=recreate"}}}, nil); err != nil {
		t.Fatal(err)
	}
	pinger := startNetPinger(t, rt, name, "http://web:8080/")
	time.Sleep(time.Second)
	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{startDelay, "strategy=recreate", "content=v2"}}}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	res := pinger.result()
	t.Logf("recreate: %s", res)
	if res.Failed <= 3*maxRaceFailures {
		t.Fatalf("recreate showed only %d failures; the pinger cannot tell an outage from the replace race: %s", res.Failed, res)
	}
}
