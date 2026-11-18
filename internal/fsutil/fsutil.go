// Package fsutil writes files so that a crash or a power loss leaves the old content or the new one,
// never a part.
package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data and mode: written to a temporary file beside it, synced,
// renamed over path, and the directory synced. Readers see the old file or the new one.
func WriteFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename; cleans up after a failure
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil { // CreateTemp makes it 0600
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// SyncDir makes the entries of dir (created, renamed or removed files) durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read-only
	return d.Sync()
}
