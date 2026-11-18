package releases

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
)

// digestJSON is the keyed digest of v's JSON: values and manifests may hold secret material.
func (s *Core) digestJSON(v any) (string, []byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return s.Deploy.Digest(js), js, nil
}

// keyFile holds the key of the state's digests of secret material.
const keyFile = "state.key"

// loadKey reads home/state.key, the digest key, creating it (32 random bytes, 0600) on first use.
// Losing it only means secrets are re-applied once.
func loadKey(home string) ([]byte, error) {
	path := filepath.Join(home, keyFile)
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			err = writeNewFile(path, key)
			if errors.Is(err, os.ErrExist) { // created meanwhile
				key, err = os.ReadFile(path)
			}
		}
	}
	if err == nil && len(key) < 16 {
		err = fmt.Errorf("%s is too short; remove it to create a new key", path)
	}
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	return key, nil
}

// writeNewFile creates path with data atomically: the data is written to a temporary file that is
// then linked into place, so a concurrent reader never sees it half written. It fails with
// os.ErrExist when path exists.
func writeNewFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// On disk before it is linked: after a power loss the file is whole or absent, never empty.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the entries of dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read-only
	return d.Sync()
}

// RegisterSecrets gives the log redactor the Secret values of every release's current revision,
// as Materialize wrote them. karga-server calls it when it starts, so values are masked before
// any release is applied again. A revision without secret files (none, or pruned) is skipped.
func (s *Core) RegisterSecrets(ctx context.Context) {
	rels, err := s.Store.ListReleases(ctx)
	if err != nil {
		s.Log.Warn("register secrets: list releases", "error", err)
		return
	}
	for _, rel := range rels {
		if rel.CurrentRevision == 0 {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(deployment.RevisionDir(s.Home, rel.Name, rel.CurrentRevision), "secrets", "*", "*"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				s.Log.Warn("register secrets: read", "release", rel.Name, "file", f, "error", err)
				continue
			}
			s.Redact(string(b))
		}
	}
}
