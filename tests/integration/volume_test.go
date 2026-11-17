//go:build integration

package integration

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Data an image keeps in its own VOLUME path (no karga Volume there) survives an upgrade that
// recreates the container, and goes with the release on uninstall.
func TestAnonymousVolumeKeptAcrossUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-anon")
	s, rt := service(t, name)
	redis := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		code, err := rt.Exec(ctx, name+"-redis", ports.ExecOptions{Cmd: append([]string{"redis-cli"}, args...), Stdout: &out, Stderr: &out})
		if err != nil || code != 0 {
			t.Fatalf("redis-cli %v: %d %v %s", args, code, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}

	res, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "testdata/anon"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "/data") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	redis("SET", "k", "kept")
	redis("SAVE")
	vol := workload(t, rt, name+"-redis").AnonymousVolumes[0].Source

	res, err = s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "testdata/anon",
		Values: packages.Options{Sets: []string{"rev=2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Plan.Changes[0]; c.Strategy != "recreate" {
		t.Fatalf("change: %+v", c)
	}
	if got := redis("GET", "k"); got != "kept" {
		t.Fatalf("after upgrade GET k = %q", got)
	}
	if st := workload(t, rt, name+"-redis"); len(st.AnonymousVolumes) != 1 || st.AnonymousVolumes[0].Source != vol {
		t.Fatalf("volume %s not moved: %+v", vol, st.AnonymousVolumes)
	}

	if err := s.Uninstall(ctx, releases.UninstallRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if err := rt.CreateVolume(ctx, ports.VolumeRequest{Name: vol}); err != nil {
		t.Fatalf("anonymous volume %s outlived the uninstall: %v", vol, err)
	}
	_ = rt.DeleteVolume(ctx, vol)
}
