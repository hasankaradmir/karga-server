// Package sqlite implements ports.StateStore on SQLite (tech design §14, ADR-004).
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	// Pure-Go SQLite driver, registered as "sqlite".
	_ "modernc.org/sqlite"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

//go:embed migrations/*.sql
var embedded embed.FS

// migrations holds migrations/NNN_*.sql, applied in name order (tests substitute it).
var migrations fs.FS = embedded

// Store is a SQLite-backed ports.StateStore.
type Store struct {
	db *sql.DB
}

var _ ports.StateStore = (*Store)(nil)

// Open opens (creating if needed) the database at path with mode 0600 and applies migrations.
// Opening and migrating hold <path>.lock: several karga processes may open a new database at once,
// and SQLite's switch to WAL does not wait for a busy database.
func Open(ctx context.Context, path string) (*Store, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	defer func() { _ = lock.Close() }() // closing releases the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock state database: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	_ = f.Close()
	// SQLite creates the -wal and -shm files with the database file's permissions.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx, path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// Backup writes a consistent copy of the database to dest (0600), which must not exist. It is safe
// while karga runs.
func (s *Store) Backup(ctx context.Context, dest string) error {
	// VACUUM INTO needs the target empty or absent; creating it first sets its mode.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	_ = f.Close()
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("backup to %s: %w", dest, err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies pending migrations. A database of a newer karga is refused rather than used with
// a schema this binary does not know; before migrating an existing database a copy of it is kept
// as <path>.bak-v<schema>.
func (s *Store) migrate(ctx context.Context, path string) error {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(names) {
		return fmt.Errorf("database written by a newer karga (schema %d, this karga knows %d); upgrade karga", version, len(names))
	}
	if version > 0 && version < len(names) {
		bak := fmt.Sprintf("%s.bak-v%d", path, version)
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			if err := s.Backup(ctx, bak); err != nil {
				return fmt.Errorf("back up before migrating: %w", err)
			}
		}
	}
	// Each step re-reads the version inside its (immediate, so exclusive) transaction: another karga
	// opening the same new database at the same time must not apply a migration twice.
	for {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var current int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
			_ = tx.Rollback()
			return err
		}
		if current >= len(names) {
			return tx.Rollback()
		}
		body, err := fs.ReadFile(migrations, "migrations/"+names[current])
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", names[current], err)
		}
		// PRAGMA does not take bind parameters; current is an int we control.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", current+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}

const timeFormat = time.RFC3339Nano

func ts(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(timeFormat, s)
	return t
}

func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ports.ErrNotFound)
	}
	return err
}

// GetRelease implements ports.StateStore.
func (s *Store) GetRelease(ctx context.Context, name string) (release.Release, error) {
	row := s.db.QueryRowContext(ctx, `SELECT name, package_name, package_version, current_revision, status, created_at, updated_at, paused
		FROM releases WHERE name = ?`, name)
	r, err := scanRelease(row)
	return r, notFound(err, "release "+name)
}

type scanner interface{ Scan(...any) error }

func scanRelease(row scanner) (release.Release, error) {
	var r release.Release
	var status, created, updated string
	if err := row.Scan(&r.Name, &r.PackageName, &r.PackageVersion, &r.CurrentRevision, &status, &created, &updated, &r.Paused); err != nil {
		return r, err
	}
	r.Status, r.CreatedAt, r.UpdatedAt = release.Status(status), parseTS(created), parseTS(updated)
	return r, nil
}

// ListReleases implements ports.StateStore.
func (s *Store) ListReleases(ctx context.Context) ([]release.Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, package_name, package_version, current_revision, status, created_at, updated_at, paused
		FROM releases ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []release.Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRelease implements ports.StateStore.
func (s *Store) SaveRelease(ctx context.Context, r release.Release) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases (name, package_name, package_version, current_revision, status, created_at, updated_at, paused)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET package_name = excluded.package_name, package_version = excluded.package_version,
			current_revision = excluded.current_revision, status = excluded.status, updated_at = excluded.updated_at,
			paused = excluded.paused`,
		r.Name, r.PackageName, r.PackageVersion, r.CurrentRevision, string(r.Status), ts(r.CreatedAt), ts(r.UpdatedAt), r.Paused)
	return err
}

// DeleteRelease implements ports.StateStore.
func (s *Store) DeleteRelease(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE name = ?`, name)
	return err
}

// CreateRevision implements ports.StateStore.
func (s *Store) CreateRevision(ctx context.Context, rev release.Revision) error {
	vals, err := json.Marshal(nonNil(rev.Values))
	if err != nil {
		return err
	}
	all, err := json.Marshal(nonNil(rev.ValuesAll))
	if err != nil {
		return err
	}
	supported, err := json.Marshal(rev.RuntimeSupported)
	if err != nil {
		return err
	}
	manifest := rev.Manifest
	if len(manifest) == 0 {
		manifest = json.RawMessage("[]")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO revisions (release, revision, package_name, package_version, runtime_supported,
		package_digest, values_digest, manifest_digest, values_json, values_all_json, manifest_json, status, description, created_at, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev.Release, rev.Number, rev.PackageName, rev.PackageVersion, string(supported), rev.PackageDigest, rev.ValuesDigest, rev.ManifestDigest,
		string(vals), string(all), string(manifest), string(rev.Status), rev.Description, ts(rev.CreatedAt), rev.Notes)
	return err
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// UpdateRevisionStatus implements ports.StateStore.
func (s *Store) UpdateRevisionStatus(ctx context.Context, rel string, number int, status release.RevisionStatus, description string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE revisions SET status = ?, description = ? WHERE release = ? AND revision = ?`,
		string(status), description, rel, number)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("revision %s/%d: %w", rel, number, ports.ErrNotFound)
	}
	return nil
}

const revisionColumns = `release, revision, package_name, package_version, runtime_supported, package_digest, values_digest, manifest_digest,
	values_json, values_all_json, manifest_json, status, description, created_at, notes`

func scanRevision(row scanner) (release.Revision, error) {
	var r release.Revision
	var supported, vals, all, manifest, status, created string
	if err := row.Scan(&r.Release, &r.Number, &r.PackageName, &r.PackageVersion, &supported, &r.PackageDigest, &r.ValuesDigest,
		&r.ManifestDigest, &vals, &all, &manifest, &status, &r.Description, &created, &r.Notes); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(supported), &r.RuntimeSupported); err != nil {
		return r, err
	}
	var err error
	if r.Values, err = decodeValues(vals); err != nil {
		return r, err
	}
	if r.ValuesAll, err = decodeValues(all); err != nil {
		return r, err
	}
	r.Manifest, r.Status, r.CreatedAt = json.RawMessage(manifest), release.RevisionStatus(status), parseTS(created)
	return r, nil
}

// decodeValues keeps integers as ints: plain json.Unmarshal would turn 1000000 into the float 1e+06.
func decodeValues(s string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return numbers(m).(map[string]any), nil
}

func numbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = numbers(c)
		}
	case []any:
		for i, c := range t {
			t[i] = numbers(c)
		}
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// GetRevision implements ports.StateStore.
func (s *Store) GetRevision(ctx context.Context, rel string, number int) (release.Revision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM revisions WHERE release = ? AND revision = ?`, rel, number)
	r, err := scanRevision(row)
	return r, notFound(err, fmt.Sprintf("revision %d of release %s", number, rel))
}

// ListRevisions implements ports.StateStore.
func (s *Store) ListRevisions(ctx context.Context, rel string) ([]release.Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+revisionColumns+` FROM revisions WHERE release = ? ORDER BY revision`, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []release.Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRevision implements ports.StateStore.
func (s *Store) DeleteRevision(ctx context.Context, rel string, number int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM revisions WHERE release = ? AND revision = ?`, rel, number)
	return err
}

// ListResources implements ports.StateStore.
func (s *Store) ListResources(ctx context.Context, rel string) ([]release.AppliedResource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, name, revision, runtime_name, spec_json, spec_digest
		FROM resources WHERE release = ? ORDER BY kind, name`, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []release.AppliedResource
	for rows.Next() {
		var r release.AppliedResource
		var spec string
		if err := rows.Scan(&r.Kind, &r.Name, &r.Revision, &r.RuntimeName, &spec, &r.Digest); err != nil {
			return nil, err
		}
		r.Spec = json.RawMessage(spec)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveResource implements ports.StateStore.
func (s *Store) SaveResource(ctx context.Context, rel string, r release.AppliedResource) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO resources (release, kind, name, revision, runtime_name, spec_json, spec_digest)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rel, r.Kind, r.Name, r.Revision, r.RuntimeName, string(r.Spec), r.Digest)
	return err
}

// DeleteResource implements ports.StateStore.
func (s *Store) DeleteResource(ctx context.Context, rel, kind, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM resources WHERE release = ? AND kind = ? AND name = ?`, rel, kind, name)
	return err
}

const operationColumns = `id, release, type, revision, status, pid, hostname, actor, steps_json, error, started_at, finished_at`

func scanOperation(row scanner) (release.Operation, error) {
	var op release.Operation
	var status, steps, started, finished string
	if err := row.Scan(&op.ID, &op.Release, &op.Type, &op.Revision, &status, &op.PID, &op.Hostname, &op.Actor, &steps, &op.Error, &started, &finished); err != nil {
		return op, err
	}
	op.Status, op.StartedAt = release.OperationStatus(status), parseTS(started)
	if finished != "" {
		op.FinishedAt = parseTS(finished)
	}
	return op, json.Unmarshal([]byte(steps), &op.Steps)
}

// BeginOperation implements ports.StateStore. The transaction is BEGIN IMMEDIATE (see DSN), so two
// karga processes cannot both see "no running operation" and start at once.
func (s *Store) BeginOperation(ctx context.Context, op release.Operation, onRunning func(release.Operation) error) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE release = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		op.Release, string(release.OperationRunning))
	running, err := scanOperation(row)
	switch {
	case err == nil:
		if onRunning == nil {
			return 0, fmt.Errorf("release %s has a running %s operation (id %d)", op.Release, running.Type, running.ID)
		}
		if err := onRunning(running); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET status = ?, finished_at = ? WHERE id = ?`,
			string(release.OperationAborted), ts(time.Now()), running.ID); err != nil {
			return 0, err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO operations (release, type, revision, status, pid, hostname, actor, steps_json, error, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '[]', '', ?, '')`,
		op.Release, op.Type, op.Revision, string(release.OperationRunning), op.PID, op.Hostname, op.Actor, ts(op.StartedAt))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func stepsJSON(steps []release.Step) (string, error) {
	if steps == nil {
		steps = []release.Step{}
	}
	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(steps)
	return strings.TrimSpace(buf.String()), err
}

// FinishOperation implements ports.StateStore.
func (s *Store) FinishOperation(ctx context.Context, op release.Operation) error {
	steps, err := stepsJSON(op.Steps)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE operations SET status = ?, steps_json = ?, error = ?, revision = ?, finished_at = ? WHERE id = ?`,
		string(op.Status), steps, op.Error, op.Revision, ts(time.Now()), op.ID)
	return err
}

// SaveOperationSteps implements ports.StateStore.
func (s *Store) SaveOperationSteps(ctx context.Context, id int64, steps []release.Step) error {
	js, err := stepsJSON(steps)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE operations SET steps_json = ? WHERE id = ?`, js, id)
	return err
}

// RunningOperation implements ports.StateStore.
func (s *Store) RunningOperation(ctx context.Context, rel string) (release.Operation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE release = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		rel, string(release.OperationRunning))
	op, err := scanOperation(row)
	return op, notFound(err, "running operation of "+rel)
}

// ListOperations implements ports.StateStore.
func (s *Store) ListOperations(ctx context.Context, rel string, afterID int64, limit int) ([]release.Operation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+operationColumns+` FROM operations
		WHERE (? = '' OR release = ?) AND id > ? ORDER BY id DESC LIMIT ?`, rel, rel, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []release.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
