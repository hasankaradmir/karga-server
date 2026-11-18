package releases

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// digestJSON is the keyed digest of v's JSON: values and manifests may hold secret material.
func (s *Service) digestJSON(v any) (string, []byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return s.Deploy.Digest(js), js, nil
}

// keyFile holds the key of the state's digests of secret material.
const keyFile = "state.key"

// loadKey reads $KARGA_HOME/state.key, creating it (32 random bytes, 0600) on first use, and makes
// it the digest key. Losing it only means secrets are re-applied once.
func (s *Service) loadKey() error {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.key == nil {
		path := filepath.Join(s.Home, keyFile)
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
			return fmt.Errorf("state key: %w", err)
		}
		s.key = key
	}
	s.Deploy.DigestKey = s.key
	return nil
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
