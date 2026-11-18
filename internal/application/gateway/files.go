package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
)

// Dir holds the gateway's configuration and admin socket.
func Dir(home string) string { return filepath.Join(home, "gateway") }

// exposeFile lists what a release exposes ([]Route). It lives in the release proxy's directory, so
// changing a host or path rewrites a file instead of recreating the proxy container.
func exposeFile(home, rel string) string { return filepath.Join(home, "proxies", rel, "expose.yaml") }

// readExpose reads what a release exposes; a release without the file exposes nothing.
func readExpose(home, rel string) ([]Route, error) {
	data, err := os.ReadFile(exposeFile(home, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Route
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", exposeFile(home, rel), err)
	}
	return out, nil
}

// writeExpose stores what a release exposes (none removes the file).
func writeExpose(home, rel string, routes []Route) error {
	if len(routes) == 0 {
		if err := os.Remove(exposeFile(home, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := yaml.Marshal(routes)
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

// Lock serializes gateway updates of releases deployed at once (release locks are per release).
func Lock(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock the gateway: %w", err)
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}

func settingsPath(home string) string { return filepath.Join(home, "gateway.yaml") }

// ReadSettings returns the stored settings, with defaults for what was never set.
func ReadSettings(home string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(settingsPath(home))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return s, err
	default:
		if err := yaml.Unmarshal(data, &s); err != nil {
			return s, fmt.Errorf("%s: %w", settingsPath(home), err)
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

// WriteSettings validates and stores settings; the next Sync applies them.
func WriteSettings(home string, s Settings) error {
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
	return writeFile(settingsPath(home), data)
}
