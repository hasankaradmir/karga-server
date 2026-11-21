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
		code, err := rt.Exec(ctx, containerName(t, rt, name, "redis"), ports.ExecOptions{Cmd: append([]string{"redis-cli"}, args...), Stdout: &out, Stderr: &out})
		if err != nil || code != 0 {
			t.Fatalf("redis-cli %v: %d %v %s", args, code, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}

	res, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "testdata/anon"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "/data") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	redis("SET", "k", "kept")
	redis("SAVE")
	vol := workload(t, rt, containerName(t, rt, name, "redis")).AnonymousVolumes[0].Source

	res, err = s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "testdata/anon",
		Values: releases.Values{Sets: packages.Sets([]string{"rev=2"}...)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Plan[0]; c.Strategy != "recreate" {
		t.Fatalf("change: %+v", c)
	}
	if got := redis("GET", "k"); got != "kept" {
		t.Fatalf("after upgrade GET k = %q", got)
	}
	if st := workload(t, rt, containerName(t, rt, name, "redis")); len(st.AnonymousVolumes) != 1 || st.AnonymousVolumes[0].Source != vol {
		t.Fatalf("volume %s not moved: %+v", vol, st.AnonymousVolumes)
	}

	if _, err := s.Lifecycle.Uninstall(ctx, releases.UninstallRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if err := rt.CreateVolume(ctx, ports.VolumeRequest{Name: vol}); err != nil {
		t.Fatalf("anonymous volume %s outlived the uninstall: %v", vol, err)
	}
	_ = rt.DeleteVolume(ctx, vol)
}

// On Docker: a backup stops the database and what depends on it, not the volume's reader; a
// restore replaces the volume's files (a file written after the backup goes), through a container
// with a read-only image, and undo-restore brings the files back.
func TestVolumeRestoreReplacesOnDocker(t *testing.T) {
	t.Parallel()
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-vol")
	s, rt := service(t, name)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "testdata/volapp"}, nil); err != nil {
		t.Fatal(err)
	}
	sh := func(workload, cmd string) string {
		t.Helper()
		var out bytes.Buffer
		code, err := rt.Exec(ctx, containerName(t, rt, name, workload), ports.ExecOptions{Cmd: []string{"sh", "-c", cmd}, Stdout: &out, Stderr: &out})
		if err != nil || code != 0 {
			t.Fatalf("%s: %s: %d %v %s", workload, cmd, code, err, out.String())
		}
		return strings.Join(strings.Fields(out.String()), " ")
	}
	sh("db", "echo a > /var/lib/db/a")

	var backup bytes.Buffer
	res, err := s.Volumes.Backup(ctx, releases.VolumeBackupRequest{Release: name, Volume: "data"}, &backup, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Stopped, ",") != "db,app" || res.Downtime <= 0 {
		t.Fatalf("backup: %+v", res)
	}
	sh("db", "echo b > /var/lib/db/b")
	rres, err := s.Volumes.Restore(ctx, releases.VolumeRestoreRequest{Release: name, Volume: "data", Name: "backup"}, &backup, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sh("db", "ls /var/lib/db"); got != "a" {
		t.Fatalf("after restore: %q", got)
	}
	if got := sh("reader", "ls /srv"); got != "a" {
		t.Fatalf("the reader sees %q", got)
	}
	if strings.Join(rres.Stopped, ",") != "db,app,reader" || rres.RestorePoint == "" {
		t.Fatalf("restore: %+v", rres)
	}
	if _, err := s.Volumes.UndoRestore(ctx, name, "data", nil); err != nil {
		t.Fatal(err)
	}
	if got := sh("db", "ls /var/lib/db"); got != "a b" {
		t.Fatalf("after undo: %q", got)
	}
}
