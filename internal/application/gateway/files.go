package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
)

// Dir holds the gateway's configuration and admin socket.
func Dir(home string) string { return filepath.Join(home, "gateway") }

// exposeFile lists what a release exposes (exposeData). It lives in the release proxy's directory, so
// changing a host or path rewrites a file instead of recreating the proxy container.
func exposeFile(home, rel string) string { return filepath.Join(home, "proxies", rel, "expose.yaml") }

// readExpose reads what a release exposes; a release without the file exposes nothing.
func readExpose(home, rel string) (string, []Route, error) {
	data, err := os.ReadFile(exposeFile(home, rel))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var f exposeData
	if err := yaml.Unmarshal(data, &f); err != nil {
		return "", nil, fmt.Errorf("%s: %w", exposeFile(home, rel), err)
	}
	return f.Proxy, f.Routes, nil
}

// exposeData is a release's expose file: the proxy container the gateway routes to, and the routes.
type exposeData struct {
	Proxy  string  `yaml:"proxy,omitempty"`
	Routes []Route `yaml:"routes"`
}

// writeExpose stores what a release exposes, through proxy (no routes removes the file).
func writeExpose(home, rel, proxy string, routes []Route) error {
	if len(routes) == 0 {
		if err := os.Remove(exposeFile(home, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := yaml.Marshal(exposeData{Proxy: proxy, Routes: routes})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(exposeFile(home, rel)), 0o700); err != nil {
		return err
	}
	return writeFile(exposeFile(home, rel), data)
}

// syncCerts copies the certificates of the targets' HTTPS routes into the gateway directory and
// removes the ones no longer used. Files are 0600; the gateway reads them as root.
func syncCerts(dir string, targets []Target, prune bool) error {
	certs := filepath.Join(dir, "certs")
	want := map[string]bool{}
	for _, t := range targets {
		for _, r := range t.Routes {
			if r.TLSSecret == "" {
				continue
			}
			name := certName(t.Release, r.Listener, r.Host, r.CertDir)
			for ext, key := range map[string]string{".crt": resource.TLSCertKey, ".key": resource.TLSKeyKey} {
				data, err := os.ReadFile(filepath.Join(r.CertDir, key))
				if err != nil {
					return fmt.Errorf("%s/%s: certificate for %s: %w", t.Release, r.Service, r.Host, err)
				}
				if err := os.MkdirAll(certs, 0o700); err != nil {
					return err
				}
				if err := writeFile(filepath.Join(certs, name+ext), data); err != nil {
					return err
				}
				want[name+ext] = true
			}
		}
	}
	if !prune {
		return nil
	}
	entries, err := os.ReadDir(certs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !want[e.Name()] {
			if err := os.Remove(filepath.Join(certs, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeFile replaces path atomically with mode 0600, leaving an identical file alone.
func writeFile(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return fsutil.WriteFile(path, data, 0o600)
}

// Lock serializes gateway updates of releases deployed at once (release locks are per release). It
// waits until the gateway is free or ctx ends.
func Lock(ctx context.Context, home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil // closing releases the lock
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock the gateway: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the gateway: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The administrator's settings are in gateway.yaml, which they may edit; karga runs on
// gateway.applied.yaml, the settings last applied successfully (D39).
func settingsPath(home string) string { return filepath.Join(home, "gateway.yaml") }
func appliedPath(home string) string  { return filepath.Join(home, "gateway.applied.yaml") }

// parseSettings reads settings as written (an unknown field is an error, so a typo is not
// ignored), with defaults for what was never set.
func parseSettings(data []byte) (Settings, error) {
	var s Settings
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&s); err != nil {
			return s, err
		}
	}
	if len(s.Listen) == 0 {
		s.Listen = []string{DefaultListen}
	}
	if s.Listeners == nil { // never set (an empty list is the administrator's choice)
		s.Listeners = defaultListeners()
	}
	return s, nil
}

// ReadSettings returns the settings the gateway runs on. On a host where none were applied yet,
// they are gateway.yaml's (or the defaults), checked like an apply would.
func ReadSettings(home string) (Settings, error) {
	path := appliedPath(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		path = settingsPath(home)
		data, err = os.ReadFile(path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Settings{}, err
	}
	s, err := parseSettings(data)
	if err == nil {
		err = validateSettings(s)
	}
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ReadEdited returns gateway.yaml as the administrator wrote it, and the digest of its content
// (an apply names it, so a file changed after its validation is not applied).
func ReadEdited(home string) (Settings, string, error) {
	data, err := os.ReadFile(settingsPath(home))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Settings{}, "", err
	}
	sum := sha256.Sum256(data)
	s, err := parseSettings(data)
	return s, "sha256:" + hex.EncodeToString(sum[:]), err
}

// WriteSettings validates settings and stores them as the administrator's and the applied ones
// (a karga gateway command changed them); the next Sync applies them.
func WriteSettings(home string, s Settings) error {
	if err := writeSettings(home, settingsPath(home), s); err != nil {
		return err
	}
	return writeSettings(home, appliedPath(home), s)
}

// writeApplied stores validated settings as the applied ones only.
func writeApplied(home string, s Settings) error { return writeSettings(home, appliedPath(home), s) }

func writeSettings(home, path string, s Settings) error {
	if err := validateSettings(s); err != nil {
		return err
	}
	if s.Listeners == nil {
		s.Listeners = []Listener{}
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return writeFile(path, data)
}
