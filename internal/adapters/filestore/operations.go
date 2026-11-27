package filestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

type operationFile struct {
	ID         int64          `yaml:"id"`
	Release    string         `yaml:"release"`
	Type       string         `yaml:"type"`
	Revision   int            `yaml:"revision"`
	Status     string         `yaml:"status"`
	PID        int            `yaml:"pid"`
	Hostname   string         `yaml:"hostname"`
	Actor      string         `yaml:"actor,omitempty"`
	Options    optionsFile    `yaml:"options,omitempty"`
	Steps      []release.Step `yaml:"steps"`
	Error      string         `yaml:"error,omitempty"`
	Downtime   time.Duration  `yaml:"downtime,omitempty"`
	StartedAt  time.Time      `yaml:"startedAt"`
	FinishedAt time.Time      `yaml:"finishedAt,omitempty"`
}

type optionsFile struct {
	DeleteVolumes bool          `yaml:"deleteVolumes,omitempty"`
	Strategy      string        `yaml:"strategy,omitempty"`
	Timeout       time.Duration `yaml:"timeout,omitempty"`
	KeepHistory   bool          `yaml:"keepHistory,omitempty"`
	HistoryMax    *int          `yaml:"historyMax,omitempty"`
	Restore       *restoreFile  `yaml:"restore,omitempty"`
}

type restoreFile struct {
	Volume         string `yaml:"volume"`
	From           string `yaml:"from"`
	Name           string `yaml:"name,omitempty"`
	Merge          bool   `yaml:"merge,omitempty"`
	NoRestorePoint bool   `yaml:"noRestorePoint,omitempty"`
	Undo           bool   `yaml:"undo,omitempty"`
}

func optionsToFile(o release.Options) optionsFile {
	f := optionsFile{DeleteVolumes: o.DeleteVolumes, Strategy: o.Strategy, Timeout: o.Timeout, KeepHistory: o.KeepHistory, HistoryMax: o.HistoryMax}
	if r := o.Restore; r != nil {
		f.Restore = &restoreFile{Volume: r.Volume, From: r.From, Name: r.Name, Merge: r.Merge, NoRestorePoint: r.NoRestorePoint, Undo: r.Undo}
	}
	return f
}

func (f optionsFile) options() release.Options {
	o := release.Options{DeleteVolumes: f.DeleteVolumes, Strategy: f.Strategy, Timeout: f.Timeout, KeepHistory: f.KeepHistory, HistoryMax: f.HistoryMax}
	if r := f.Restore; r != nil {
		o.Restore = &release.RestoreOptions{Volume: r.Volume, From: r.From, Name: r.Name, Merge: r.Merge, NoRestorePoint: r.NoRestorePoint, Undo: r.Undo}
	}
	return o
}

func toFile(op release.Operation) operationFile {
	steps := op.Steps
	if steps == nil {
		steps = []release.Step{}
	}
	return operationFile{ID: op.ID, Release: op.Release, Type: op.Type, Revision: op.Revision, Status: string(op.Status), PID: op.PID,
		Hostname: op.Hostname, Actor: op.Actor, Options: optionsToFile(op.Options), Steps: steps, Error: op.Error, Downtime: op.Downtime, StartedAt: op.StartedAt, FinishedAt: op.FinishedAt}
}

func (f operationFile) operation() release.Operation {
	return release.Operation{ID: f.ID, Release: f.Release, Type: f.Type, Revision: f.Revision, Status: release.OperationStatus(f.Status),
		PID: f.PID, Hostname: f.Hostname, Actor: f.Actor, Options: f.Options.options(), Steps: f.Steps, Error: f.Error, Downtime: f.Downtime, StartedAt: f.StartedAt, FinishedAt: f.FinishedAt}
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

// updateOperation changes the operation id of rel with fn. Only rel's file is read: another
// release's, even unreadable, does not stand in the way.
func (s *Store) updateOperation(rel string, id int64, fn func(*release.Operation)) error {
	if err := checkRelease(rel); err != nil {
		return err
	}
	if s.tx != nil {
		return errors.New("operations cannot change inside InTx")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	return fmt.Errorf("operation %d of %s: %w", id, rel, ports.ErrNotFound)
}

// BeginOperation implements ports.StateStore.
func (s *Store) BeginOperation(_ context.Context, op release.Operation, onRunning func(release.Operation) error) (int64, error) {
	if err := checkRelease(op.Release); err != nil {
		return 0, err
	}
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
	return s.updateOperation(op.Release, op.ID, func(o *release.Operation) {
		o.Status, o.Steps, o.Error, o.Revision, o.Downtime, o.FinishedAt = op.Status, op.Steps, op.Error, op.Revision, op.Downtime, time.Now()
	})
}

// SaveOperationSteps implements ports.StateStore.
func (s *Store) SaveOperationSteps(_ context.Context, rel string, id int64, steps []release.Step) error {
	return s.updateOperation(rel, id, func(o *release.Operation) { o.Steps = steps })
}

// RunningOperation implements ports.StateStore.
func (s *Store) RunningOperation(_ context.Context, rel string) (release.Operation, error) {
	if err := checkRelease(rel); err != nil {
		return release.Operation{}, err
	}
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
	if rel != "" {
		if err := checkRelease(rel); err != nil {
			return nil, err
		}
	}
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
		if err != nil && rel == "" {
			continue // every release's: an unreadable file hides its own only ('karga events <release>' says why)
		}
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
