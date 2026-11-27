// Package filestore implements ports.StateStore with YAML files under $KARGA_HOME/state (ADR-0017).
//
//	state/version.yaml                         format version
//	state/next-operation.yaml                  the next operation id
//	state/releases/<release>/release.yaml      the release
//	state/releases/<release>/revisions/<n>.yaml
//	state/releases/<release>/resources/<kind>_<name>.yaml
//	state/operations/<release>.yaml            its operations, one YAML document each, oldest first
//
// Every file is replaced atomically (written beside it, synced, renamed). A transaction (InTx) keeps
// its writes in memory and applies them at the end, the release file last: until it is written a
// release still points at what it had, so a crash in between leaves at most unreferenced files.
package filestore

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// formatVersion is the layout this package reads and writes.
const formatVersion = 1

// keepOperations is how many operations a release keeps; older ones are dropped as new ones finish.
const keepOperations = 200

// Store is a file-backed ports.StateStore.
type Store struct {
	root string
	// mu serializes writes: the state of every release, and the operation counter.
	mu *sync.Mutex
	tx *overlay // the writes of a transaction in progress, nil outside InTx
}

var _ ports.StateStore = (*Store)(nil)

// Open opens the state under home, creating it on first use. A state database of an earlier build
// (karga.db) is refused: its contents are not converted.
func Open(home string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(home, "karga.db")); err == nil {
		return nil, fmt.Errorf("%s holds the state database of an earlier karga build, which this one does not read: remove %s to start afresh",
			filepath.Join(home, "karga.db"), home)
	}
	root := filepath.Join(home, "state")
	for _, d := range []string{root, filepath.Join(root, "releases"), filepath.Join(root, "operations")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{root: root, mu: lockFor(root)}
	var v struct {
		Version int `yaml:"version"`
	}
	switch err := s.readYAML("version.yaml", &v); {
	case errors.Is(err, fs.ErrNotExist):
		v.Version = formatVersion
		if err := s.write("version.yaml", v); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case v.Version > formatVersion:
		return nil, fmt.Errorf("state written by a newer karga-server (format %d, this one knows %d); upgrade karga-server", v.Version, formatVersion)
	}
	// Under the store's mutex: no write of this process is in progress (another process may not use
	// the home: karga-server's lock file).
	s.mu.Lock()
	err := removeTemporary(root)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// locks holds one mutex per state directory: every Store opened on the same directory in this
// process shares it (one karga-server per home holds the home's file lock against other processes).
var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

func lockFor(root string) *sync.Mutex {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	locksMu.Lock()
	defer locksMu.Unlock()
	if locks[root] == nil {
		locks[root] = &sync.Mutex{}
	}
	return locks[root]
}

// Close implements io.Closer; there is nothing to release.
func (s *Store) Close() error { return nil }

func (s *Store) path(name string) string { return filepath.Join(s.root, filepath.FromSlash(name)) }

// write stores v as YAML at name (relative to the state root): at once, or at the end of the transaction.
func (s *Store) write(name string, v any) error {
	data, err := encode(v)
	if err != nil {
		return err
	}
	if s.tx != nil {
		s.tx.set(name, data)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit(name, data)
}

// remove deletes the file name (data nil) or the directory dir.
func (s *Store) remove(name string, dir bool) error {
	if s.tx != nil {
		if dir {
			s.tx.removed = append(s.tx.removed, name)
		} else {
			s.tx.set(name, nil)
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir {
		if err := os.RemoveAll(s.path(name)); err != nil {
			return err
		}
		return fsutil.SyncDir(filepath.Dir(s.path(name)))
	}
	return s.commit(name, nil)
}

// commit replaces the file name with data (nil removes it), durably.
func (s *Store) commit(name string, data []byte) error {
	p := s.path(name)
	if data == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return fsutil.SyncDir(filepath.Dir(p))
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFile(p, data, 0o600)
}

// removeTemporary removes the temporary files a crash left in the state: a file is written beside
// itself (fsutil.WriteFile) and renamed, so one whose rename did not happen is never read again.
func removeTemporary(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), ".") || !strings.Contains(d.Name(), ".tmp-") {
			return err
		}
		return os.Remove(p)
	})
}

// read returns the file name, as the transaction sees it.
func (s *Store) read(name string) ([]byte, error) {
	if s.tx != nil {
		if data, ok := s.tx.files[name]; ok {
			if data == nil {
				return nil, fs.ErrNotExist
			}
			return data, nil
		}
		if s.tx.gone(name) {
			return nil, fs.ErrNotExist
		}
	}
	return os.ReadFile(s.path(name))
}

func (s *Store) readYAML(name string, v any) error {
	data, err := s.read(name)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", s.path(name), err)
	}
	return nil
}

// list returns the names of the files in dir, as the transaction sees them, sorted.
func (s *Store) list(dir string) ([]string, error) {
	seen := map[string]bool{}
	if s.tx == nil || !s.tx.gone(dir) {
		entries, err := os.ReadDir(s.path(dir))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				seen[e.Name()] = true
			}
		}
	}
	if s.tx != nil {
		for name, data := range s.tx.files {
			if path.Dir(name) == dir {
				seen[path.Base(name)] = data != nil
			}
		}
	}
	var out []string
	for n, ok := range seen {
		if ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func notFound(err error, what string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", what, ports.ErrNotFound)
	}
	return err
}

// checkRelease refuses a name that is not a release's: names build paths of the state, so one like
// "../x" would reach outside it. The API checks names too; the store does not rely on it.
func checkRelease(name string) error {
	if !resource.IsDNSLabel(name) {
		return fmt.Errorf("%w: %q is not a release name", ports.ErrInvalid, name)
	}
	return nil
}

func releaseDir(name string) string { return "releases/" + name }
