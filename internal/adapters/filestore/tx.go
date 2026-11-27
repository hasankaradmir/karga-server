package filestore

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// InTx implements ports.StateStore.
func (s *Store) InTx(_ context.Context, fn func(ports.StateStore) error) error {
	if s.tx != nil {
		return fn(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Store{root: s.root, mu: s.mu, tx: &overlay{files: map[string][]byte{}}}
	if err := fn(tx); err != nil {
		return err
	}
	return s.apply(tx.tx)
}

// overlay holds a transaction's writes: file contents, nil for a removed file, and removed directories.
type overlay struct {
	files   map[string][]byte
	order   []string // first write of each file, to apply them in that order
	removed []string // directories, removed before any file is written
}

func (o *overlay) set(name string, data []byte) {
	if _, seen := o.files[name]; !seen {
		o.order = append(o.order, name)
	}
	o.files[name] = data
}

// gone reports whether name is in a directory the transaction removed.
func (o *overlay) gone(name string) bool {
	for _, d := range o.removed {
		if name == d || strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

// apply writes a transaction's changes, release files last.
func (s *Store) apply(o *overlay) error {
	for _, d := range o.removed {
		if err := os.RemoveAll(s.path(d)); err != nil {
			return err
		}
		if err := fsutil.SyncDir(filepath.Dir(s.path(d))); err != nil {
			return err
		}
	}
	names := append([]string(nil), o.order...)
	sort.SliceStable(names, func(i, j int) bool { return !isReleaseFile(names[i]) && isReleaseFile(names[j]) })
	for _, n := range names {
		if err := s.commit(n, o.files[n]); err != nil {
			return err
		}
	}
	return nil
}

func isReleaseFile(name string) bool { return path.Base(name) == "release.yaml" }
