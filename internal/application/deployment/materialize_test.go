package deployment

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hasankaradmir/karga-sdk/resource"
)

func cfgRes(name string, files map[string]string) resource.Resource {
	return resource.Resource{Kind: resource.KindConfig, Metadata: resource.Metadata{Name: name}, Spec: &resource.ConfigSpec{Files: files}}
}

func secretRes(name string, values map[string]string) resource.Resource {
	return resource.Resource{Kind: resource.KindSecret, Metadata: resource.Metadata{Name: name}, Spec: &resource.SecretSpec{Values: values}}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestMaterialize(t *testing.T) {
	home := t.TempDir()
	var redacted []string
	rs := []resource.Resource{
		cfgRes("redis-config", map[string]string{"redis.conf": "maxmemory 1mb\n"}),
		secretRes("db", map[string]string{"password": "hunter2", "user": "app"}),
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "data"}, Spec: &resource.VolumeSpec{}},
	}
	paths, err := Materialize(home, "web", 3, rs, func(s string) { redacted = append(redacted, s) })
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, "releases", "web", "3")
	cfgDir := filepath.Join(base, "configs", "redis-config")
	secDir := filepath.Join(base, "secrets", "db")
	if paths[Key{resource.KindConfig, "redis-config"}] != cfgDir || paths[Key{resource.KindSecret, "db"}] != secDir || len(paths) != 2 {
		t.Fatalf("paths: %v", paths)
	}
	if b, _ := os.ReadFile(filepath.Join(secDir, "password")); string(b) != "hunter2" {
		t.Fatalf("secret content: %q", b)
	}
	for p, want := range map[string]os.FileMode{
		filepath.Join(home, "releases"):        0o700,
		filepath.Join(home, "releases", "web"): 0o700,
		base:                                   0o700,
		filepath.Join(base, "secrets"):         0o700,
		secDir:                                 0o755,
		filepath.Join(secDir, "password"):      0o644,
		filepath.Join(cfgDir, "redis.conf"):    0o644,
	} {
		if got := mode(t, p); got != want {
			t.Errorf("%s: mode %o, want %o", p, got, want)
		}
	}
	slices.Sort(redacted)
	if !slices.Equal(redacted, []string{"app", "hunter2"}) {
		t.Fatalf("redacted: %v", redacted)
	}
}

func TestMaterializeUpdatesInPlace(t *testing.T) {
	home := t.TempDir()
	noop := func(string) {}
	if _, err := Materialize(home, "web", 1, []resource.Resource{cfgRes("c", map[string]string{"old": "x", "same": "s"}), cfgRes("gone", map[string]string{"a": "b"})}, noop); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "releases", "web", "1", "configs", "c")
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	same, err := os.Stat(filepath.Join(dir, "same"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(home, "web", 1, []resource.Resource{cfgRes("c", map[string]string{"new": "y", "same": "s"})}, noop); err != nil {
		t.Fatal(err)
	}
	// Running containers bind mount the directory: it must be the same one, not a new one.
	after, err := os.Stat(dir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the mounted directory was replaced: %v", err)
	}
	if again, err := os.Stat(filepath.Join(dir, "same")); err != nil || !os.SameFile(same, again) {
		t.Fatalf("an unchanged file was rewritten: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "new")); err != nil || string(b) != "y" || mode(t, filepath.Join(dir, "new")) != 0o644 {
		t.Fatalf("new file: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, "releases", "web", "1", "configs", "gone")); !os.IsNotExist(err) {
		t.Fatalf("removed Config kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "releases", "web", "1", "configs", "c", "old")); !os.IsNotExist(err) {
		t.Fatalf("stale file kept: %v", err)
	}
}
