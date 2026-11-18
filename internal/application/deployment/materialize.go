// Package deployment turns a rendered manifest into runtime objects.
package deployment

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
)

// Key identifies a resource inside a release.
type Key struct {
	Kind string
	Name string
}

func keyOf(r resource.Resource) Key { return Key{Kind: r.Kind, Name: r.Metadata.Name} }

// File modes for materialized Config and Secret files. The revision directory is private to the
// karga user; the mounted directory and files are world-readable so any uid inside the container
// can read them (bind mounts keep host ownership).
const (
	privateDirMode = 0o700
	mountDirMode   = 0o755
	mountFileMode  = 0o644
)

// RevisionDir is where a revision's Config and Secret files live.
func RevisionDir(home, release string, revision int) string {
	return filepath.Join(home, "releases", release, strconv.Itoa(revision))
}

// Materialize writes Config and Secret resources to host directories and returns the directory per
// resource for bind mounting. Every secret value is passed to redact so it never reaches logs.
//
// The directories are updated in place, never removed and recreated: running containers bind mount
// them, and a container keeps the directory it mounted, so a recreated one would leave them with an
// empty, deleted directory. Changed files are replaced atomically; files and resources the manifest
// no longer has are removed.
func Materialize(home, release string, revision int, rs []resource.Resource, redact func(string)) (map[Key]string, error) {
	base := RevisionDir(home, release, revision)
	for _, d := range []string{filepath.Join(home, "releases"), filepath.Dir(base), base} {
		if err := mkdirMode(d, privateDirMode); err != nil {
			return nil, err
		}
	}

	paths := map[Key]string{}
	keep := map[string]map[string]bool{"configs": {}, "secrets": {}}
	for _, r := range rs {
		var sub string
		var files map[string]string
		switch s := r.Spec.(type) {
		case *resource.ConfigSpec:
			sub, files = "configs", s.Files
		case *resource.SecretSpec:
			sub, files = "secrets", s.Values
			for _, v := range s.Values {
				redact(v)
			}
		default:
			continue
		}
		if err := mkdirMode(filepath.Join(base, sub), privateDirMode); err != nil {
			return nil, err
		}
		dir := filepath.Join(base, sub, r.Metadata.Name)
		if err := mkdirMode(dir, mountDirMode); err != nil {
			return nil, err
		}
		if err := syncDir(dir, files); err != nil {
			return nil, fmt.Errorf("write %s %s: %w", r.Kind, r.Metadata.Name, err)
		}
		keep[sub][r.Metadata.Name] = true
		paths[keyOf(r)] = dir
	}
	for sub, names := range keep {
		if err := removeOthers(filepath.Join(base, sub), names); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// syncDir makes dir hold exactly files: unchanged files are left alone, changed ones are replaced
// atomically (written next to them, then renamed over them) and others are removed.
func syncDir(dir string, files map[string]string) error {
	for name, content := range files {
		p := filepath.Join(dir, name)
		if old, err := os.ReadFile(p); err == nil && string(old) == content {
			if err := os.Chmod(p, mountFileMode); err != nil {
				return err
			}
			continue
		}
		if err := fsutil.WriteFile(p, []byte(content), mountFileMode); err != nil {
			return err
		}
	}
	keep := map[string]bool{}
	for name := range files {
		keep[name] = true
	}
	return removeOthers(dir, keep)
}

// removeOthers removes the entries of dir that keep does not name; a missing dir has none.
func removeOthers(dir string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// mkdirMode creates dir if needed and sets its mode regardless of umask.
func mkdirMode(dir string, mode os.FileMode) error {
	if err := os.Mkdir(dir, mode); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chmod(dir, mode)
}
