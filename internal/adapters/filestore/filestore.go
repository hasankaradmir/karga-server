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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
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
		return os.RemoveAll(s.path(name))
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
		return syncDir(filepath.Dir(p))
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read-only
	return d.Sync()
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

func releaseDir(name string) string { return "releases/" + name }

// --- releases ---

type releaseFile struct {
	Name            string    `yaml:"name"`
	PackageName     string    `yaml:"package"`
	PackageVersion  string    `yaml:"version"`
	CurrentRevision int       `yaml:"revision"`
	Status          string    `yaml:"status"`
	CreatedAt       time.Time `yaml:"createdAt"`
	UpdatedAt       time.Time `yaml:"updatedAt"`
	Paused          bool      `yaml:"paused,omitempty"`
}

func (f releaseFile) release() release.Release {
	return release.Release{Name: f.Name, PackageName: f.PackageName, PackageVersion: f.PackageVersion, CurrentRevision: f.CurrentRevision,
		Status: release.Status(f.Status), CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt, Paused: f.Paused}
}

// GetRelease implements ports.StateStore.
func (s *Store) GetRelease(_ context.Context, name string) (release.Release, error) {
	var f releaseFile
	if err := s.readYAML(releaseDir(name)+"/release.yaml", &f); err != nil {
		return release.Release{}, notFound(err, "release "+name)
	}
	return f.release(), nil
}

// ListReleases implements ports.StateStore. Releases come sorted by name.
func (s *Store) ListReleases(ctx context.Context) ([]release.Release, error) {
	names, err := s.dirs("releases")
	if err != nil {
		return nil, err
	}
	var out []release.Release
	for _, n := range names {
		r, err := s.GetRelease(ctx, n)
		if errors.Is(err, ports.ErrNotFound) {
			continue // revisions written by a transaction that did not reach its release file
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// dirs returns the directories in dir, as the transaction sees them, sorted.
func (s *Store) dirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(s.path(dir))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && (s.tx == nil || !s.tx.gone(dir+"/"+e.Name())) {
			seen[e.Name()] = true
		}
	}
	if s.tx != nil {
		for name, data := range s.tx.files {
			if rest, ok := strings.CutPrefix(name, dir+"/"); ok && data != nil {
				seen[strings.SplitN(rest, "/", 2)[0]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// SaveRelease implements ports.StateStore. CreatedAt is kept from the stored release.
func (s *Store) SaveRelease(ctx context.Context, r release.Release) error {
	if old, err := s.GetRelease(ctx, r.Name); err == nil {
		r.CreatedAt = old.CreatedAt
	}
	return s.write(releaseDir(r.Name)+"/release.yaml", releaseFile{Name: r.Name, PackageName: r.PackageName, PackageVersion: r.PackageVersion,
		CurrentRevision: r.CurrentRevision, Status: string(r.Status), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Paused: r.Paused})
}

// DeleteRelease implements ports.StateStore: the release with its revisions and resources. Its
// operations stay, like an audit log.
func (s *Store) DeleteRelease(_ context.Context, name string) error {
	return s.remove(releaseDir(name), true)
}

// --- revisions ---

type revisionFile struct {
	Release          string         `yaml:"release"`
	Number           int            `yaml:"revision"`
	PackageName      string         `yaml:"package"`
	PackageVersion   string         `yaml:"version"`
	RuntimeSupported []string       `yaml:"runtimeSupported"`
	PackageDigest    string         `yaml:"packageDigest"`
	ValuesDigest     string         `yaml:"valuesDigest"`
	ManifestDigest   string         `yaml:"manifestDigest"`
	Values           map[string]any `yaml:"values"`
	ValuesAll        map[string]any `yaml:"valuesAll"`
	// Manifest is the rendered resources, secret values replaced by digests.
	Manifest    []any     `yaml:"manifest"`
	Notes       string    `yaml:"notes,omitempty"`
	Status      string    `yaml:"status"`
	Description string    `yaml:"description"`
	CreatedAt   time.Time `yaml:"createdAt"`
}

func revisionPath(rel string, n int) string {
	return releaseDir(rel) + "/revisions/" + strconv.Itoa(n) + ".yaml"
}

// CreateRevision implements ports.StateStore.
func (s *Store) CreateRevision(_ context.Context, rev release.Revision) error {
	var manifest []any
	if len(rev.Manifest) > 0 {
		if err := json.Unmarshal(rev.Manifest, &manifest); err != nil {
			return fmt.Errorf("revision %d manifest: %w", rev.Number, err)
		}
	}
	return s.write(revisionPath(rev.Release, rev.Number), revisionFile{
		Release: rev.Release, Number: rev.Number, PackageName: rev.PackageName, PackageVersion: rev.PackageVersion,
		RuntimeSupported: rev.RuntimeSupported, PackageDigest: rev.PackageDigest, ValuesDigest: rev.ValuesDigest, ManifestDigest: rev.ManifestDigest,
		Values: nonNil(rev.Values), ValuesAll: nonNil(rev.ValuesAll), Manifest: nonNilList(manifest), Notes: rev.Notes,
		Status: string(rev.Status), Description: rev.Description, CreatedAt: rev.CreatedAt,
	})
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nonNilList(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

func (f revisionFile) revision() (release.Revision, error) {
	manifest, err := json.Marshal(nonNilList(f.Manifest))
	if err != nil {
		return release.Revision{}, fmt.Errorf("revision %d manifest: %w", f.Number, err)
	}
	return release.Revision{Release: f.Release, Number: f.Number, PackageName: f.PackageName, PackageVersion: f.PackageVersion,
		RuntimeSupported: f.RuntimeSupported, PackageDigest: f.PackageDigest, ValuesDigest: f.ValuesDigest, ManifestDigest: f.ManifestDigest,
		Values: nonNil(f.Values), ValuesAll: nonNil(f.ValuesAll), Manifest: manifest, Notes: f.Notes,
		Status: release.RevisionStatus(f.Status), Description: f.Description, CreatedAt: f.CreatedAt}, nil
}

// UpdateRevisionStatus implements ports.StateStore.
func (s *Store) UpdateRevisionStatus(_ context.Context, rel string, number int, status release.RevisionStatus, description string) error {
	var f revisionFile
	if err := s.readYAML(revisionPath(rel, number), &f); err != nil {
		return notFound(err, fmt.Sprintf("revision %s/%d", rel, number))
	}
	f.Status, f.Description = string(status), description
	return s.write(revisionPath(rel, number), f)
}

// GetRevision implements ports.StateStore.
func (s *Store) GetRevision(_ context.Context, rel string, number int) (release.Revision, error) {
	var f revisionFile
	if err := s.readYAML(revisionPath(rel, number), &f); err != nil {
		return release.Revision{}, notFound(err, fmt.Sprintf("revision %d of release %s", number, rel))
	}
	return f.revision()
}

// ListRevisions implements ports.StateStore. Revisions come in ascending order.
func (s *Store) ListRevisions(ctx context.Context, rel string) ([]release.Revision, error) {
	names, err := s.list(releaseDir(rel) + "/revisions")
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, n := range names {
		if i, err := strconv.Atoi(strings.TrimSuffix(n, ".yaml")); err == nil {
			numbers = append(numbers, i)
		}
	}
	sort.Ints(numbers)
	out := make([]release.Revision, 0, len(numbers))
	for _, n := range numbers {
		r, err := s.GetRevision(ctx, rel, n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteRevision implements ports.StateStore.
func (s *Store) DeleteRevision(_ context.Context, rel string, number int) error {
	return s.remove(revisionPath(rel, number), false)
}

// --- applied resources ---

type resourceFile struct {
	Kind        string `yaml:"kind"`
	Name        string `yaml:"name"`
	Revision    int    `yaml:"revision"`
	RuntimeName string `yaml:"runtimeName"`
	// Spec is the canonical redacted spec; Digest covers it plus mounted content.
	Spec   any    `yaml:"spec"`
	Digest string `yaml:"digest"`
}

func resourcePath(rel, kind, name string) string {
	return releaseDir(rel) + "/resources/" + kind + "_" + name + ".yaml"
}

// ListResources implements ports.StateStore. Resources come sorted by kind and name.
func (s *Store) ListResources(_ context.Context, rel string) ([]release.AppliedResource, error) {
	dir := releaseDir(rel) + "/resources"
	names, err := s.list(dir)
	if err != nil {
		return nil, err
	}
	var out []release.AppliedResource
	for _, n := range names {
		var f resourceFile
		if err := s.readYAML(dir+"/"+n, &f); err != nil {
			return nil, err
		}
		spec, err := json.Marshal(f.Spec)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		out = append(out, release.AppliedResource{Kind: f.Kind, Name: f.Name, Revision: f.Revision, RuntimeName: f.RuntimeName, Spec: spec, Digest: f.Digest})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// SaveResource implements ports.StateStore.
func (s *Store) SaveResource(_ context.Context, rel string, r release.AppliedResource) error {
	var spec any
	if len(r.Spec) > 0 {
		if err := json.Unmarshal(r.Spec, &spec); err != nil {
			return fmt.Errorf("%s %s spec: %w", r.Kind, r.Name, err)
		}
	}
	return s.write(resourcePath(rel, r.Kind, r.Name), resourceFile{Kind: r.Kind, Name: r.Name, Revision: r.Revision,
		RuntimeName: r.RuntimeName, Spec: spec, Digest: r.Digest})
}

// DeleteResource implements ports.StateStore.
func (s *Store) DeleteResource(_ context.Context, rel, kind, name string) error {
	return s.remove(resourcePath(rel, kind, name), false)
}

// --- operations ---

type operationFile struct {
	ID         int64          `yaml:"id"`
	Release    string         `yaml:"release"`
	Type       string         `yaml:"type"`
	Revision   int            `yaml:"revision"`
	Status     string         `yaml:"status"`
	PID        int            `yaml:"pid"`
	Hostname   string         `yaml:"hostname"`
	Actor      string         `yaml:"actor,omitempty"`
	Steps      []release.Step `yaml:"steps"`
	Error      string         `yaml:"error,omitempty"`
	StartedAt  time.Time      `yaml:"startedAt"`
	FinishedAt time.Time      `yaml:"finishedAt,omitempty"`
}

func toFile(op release.Operation) operationFile {
	steps := op.Steps
	if steps == nil {
		steps = []release.Step{}
	}
	return operationFile{ID: op.ID, Release: op.Release, Type: op.Type, Revision: op.Revision, Status: string(op.Status), PID: op.PID,
		Hostname: op.Hostname, Actor: op.Actor, Steps: steps, Error: op.Error, StartedAt: op.StartedAt, FinishedAt: op.FinishedAt}
}

func (f operationFile) operation() release.Operation {
	return release.Operation{ID: f.ID, Release: f.Release, Type: f.Type, Revision: f.Revision, Status: release.OperationStatus(f.Status),
		PID: f.PID, Hostname: f.Hostname, Actor: f.Actor, Steps: f.Steps, Error: f.Error, StartedAt: f.StartedAt, FinishedAt: f.FinishedAt}
}

func operationsPath(rel string) string { return "operations/" + rel + ".yaml" }

// operations reads a release's operations, oldest first.
func (s *Store) operations(rel string) ([]release.Operation, error) {
	data, err := s.read(operationsPath(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []release.Operation
	for {
		var f operationFile
		if err := dec.Decode(&f); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", s.path(operationsPath(rel)), err)
		}
		out = append(out, f.operation())
	}
}

// saveOperations replaces a release's operations, keeping the newest keepOperations.
func (s *Store) saveOperations(rel string, ops []release.Operation) error {
	if len(ops) > keepOperations {
		ops = ops[len(ops)-keepOperations:]
	}
	var buf bytes.Buffer
	for _, op := range ops {
		data, err := encode(toFile(op))
		if err != nil {
			return err
		}
		buf.WriteString("---\n")
		buf.Write(data)
	}
	return s.commit(operationsPath(rel), buf.Bytes())
}

// updateOperation changes the operation id of rel with fn.
func (s *Store) updateOperation(id int64, fn func(*release.Operation)) error {
	if s.tx != nil {
		return errors.New("operations cannot change inside InTx")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rels, err := s.list("operations")
	if err != nil {
		return err
	}
	for _, f := range rels {
		rel := strings.TrimSuffix(f, ".yaml")
		ops, err := s.operations(rel)
		if err != nil {
			return err
		}
		for i := range ops {
			if ops[i].ID == id {
				fn(&ops[i])
				return s.saveOperations(rel, ops)
			}
		}
	}
	return fmt.Errorf("operation %d: %w", id, ports.ErrNotFound)
}

// BeginOperation implements ports.StateStore.
func (s *Store) BeginOperation(_ context.Context, op release.Operation, onRunning func(release.Operation) error) (int64, error) {
	if s.tx != nil {
		return 0, errors.New("BeginOperation cannot run inside InTx")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := s.operations(op.Release)
	if err != nil {
		return 0, err
	}
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].Status != release.OperationRunning {
			continue
		}
		if onRunning == nil {
			return 0, fmt.Errorf("release %s has a running %s operation (id %d)", op.Release, ops[i].Type, ops[i].ID)
		}
		if err := onRunning(ops[i]); err != nil {
			return 0, err
		}
		ops[i].Status, ops[i].FinishedAt = release.OperationAborted, time.Now()
		break
	}
	var next struct {
		ID int64 `yaml:"id"`
	}
	if err := s.readYAML("next-operation.yaml", &next); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if next.ID == 0 {
		next.ID = 1
	}
	op.ID, op.Status = next.ID, release.OperationRunning
	next.ID++
	data, err := encode(next)
	if err != nil {
		return 0, err
	}
	// The counter goes first: a crash before the operation is written only skips an id.
	if err := s.commit("next-operation.yaml", data); err != nil {
		return 0, err
	}
	return op.ID, s.saveOperations(op.Release, append(ops, op))
}

// FinishOperation implements ports.StateStore.
func (s *Store) FinishOperation(_ context.Context, op release.Operation) error {
	return s.updateOperation(op.ID, func(o *release.Operation) {
		o.Status, o.Steps, o.Error, o.Revision, o.FinishedAt = op.Status, op.Steps, op.Error, op.Revision, time.Now()
	})
}

// SaveOperationSteps implements ports.StateStore.
func (s *Store) SaveOperationSteps(_ context.Context, id int64, steps []release.Step) error {
	return s.updateOperation(id, func(o *release.Operation) { o.Steps = steps })
}

// RunningOperation implements ports.StateStore.
func (s *Store) RunningOperation(_ context.Context, rel string) (release.Operation, error) {
	ops, err := s.operations(rel)
	if err != nil {
		return release.Operation{}, err
	}
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].Status == release.OperationRunning {
			return ops[i], nil
		}
	}
	return release.Operation{}, fmt.Errorf("running operation of %s: %w", rel, ports.ErrNotFound)
}

// ListOperations implements ports.StateStore: newest first, at most limit (default 100), only those
// with an id above afterID, of release rel or of every release ("").
func (s *Store) ListOperations(_ context.Context, rel string, afterID int64, limit int) ([]release.Operation, error) {
	if limit <= 0 {
		limit = 100
	}
	files := []string{rel + ".yaml"}
	if rel == "" {
		var err error
		if files, err = s.list("operations"); err != nil {
			return nil, err
		}
	}
	var out []release.Operation
	for _, f := range files {
		ops, err := s.operations(strings.TrimSuffix(f, ".yaml"))
		if err != nil {
			return nil, err
		}
		for _, op := range ops {
			if op.ID > afterID {
				out = append(out, op)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
