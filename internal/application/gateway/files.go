package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/fsutil"
	"github.com/hasankaradmir/karga-server/internal/ports"
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
// readableCerts checks that t's TLS certificates can be read (syncCerts copies them).
func readableCerts(t Target) error {
	for _, r := range t.Routes {
		if r.TLSSecret == "" {
			continue
		}
		for _, key := range []string{resource.TLSCertKey, resource.TLSKeyKey} {
			if _, err := os.Stat(filepath.Join(r.CertDir, key)); err != nil {
				return fmt.Errorf("%s/%s: certificate for %s: %w", t.Release, r.Service, r.Host, err)
			}
		}
	}
	return nil
}

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

// The gateway settings are revisions karga keeps (D40): $KARGA_HOME/gateway-settings/<n>.yaml,
// each a document (kind Gateway), and history.yaml, who applied each and how. The highest is in
// force; the last keepRevisions are kept.
func settingsDir(home string) string { return filepath.Join(home, "gateway-settings") }
func historyPath(home string) string { return filepath.Join(settingsDir(home), "history.yaml") }
func revisionPath(home string, n int) string {
	return filepath.Join(settingsDir(home), strconv.Itoa(n)+".yaml")
}

const keepRevisions = 10

// Document header.
const (
	DocumentAPIVersion = resource.APIVersion
	DocumentKind       = "Gateway"
)

type document struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Spec       Settings `yaml:"spec"`
}

// ParseDocument reads a gateway document as the user wrote it: an unknown field, apiVersion or
// kind is an error (a typo is not ignored); what is not set takes its default. It does not
// validate the values (validateSettings).
func ParseDocument(data []byte) (Settings, error) {
	var d document
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&d); err != nil {
			return Settings{}, err
		}
	}
	switch {
	case d.APIVersion != DocumentAPIVersion:
		return Settings{}, fmt.Errorf("apiVersion %q: want %s", d.APIVersion, DocumentAPIVersion)
	case d.Kind != DocumentKind:
		return Settings{}, fmt.Errorf("kind %q: want %s", d.Kind, DocumentKind)
	}
	return withDefaults(d.Spec), nil
}

func withDefaults(s Settings) Settings {
	if len(s.Listen) == 0 {
		s.Listen = []string{DefaultListen}
	}
	if s.Listeners == nil { // never set (an empty list is the administrator's choice)
		s.Listeners = defaultListeners()
	}
	return s
}

// Document writes settings as a gateway document.
func Document(s Settings) []byte {
	if s.Listeners == nil {
		s.Listeners = []Listener{}
	}
	data, _ := yaml.Marshal(document{APIVersion: DocumentAPIVersion, Kind: DocumentKind, Spec: s}) // plain structs
	return data
}

// SettingsRevision is one applied revision: when, by whom, from what ("apply", "listener add pg").
type SettingsRevision struct {
	Revision int       `yaml:"revision"`
	Time     time.Time `yaml:"time"`
	Actor    string    `yaml:"actor,omitempty"`
	Source   string    `yaml:"source"`
}

// Revisions lists the revisions kept, oldest first.
func Revisions(home string) ([]SettingsRevision, error) {
	data, err := os.ReadFile(historyPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SettingsRevision
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", historyPath(home), err)
	}
	return out, nil
}

// RevisionSettings returns revision n (0: the one in force, or the defaults when none was
// applied).
func RevisionSettings(home string, n int) (Settings, int, error) {
	if n == 0 {
		revs, err := Revisions(home)
		if err != nil || len(revs) == 0 {
			return withDefaults(Settings{}), 0, err
		}
		n = revs[len(revs)-1].Revision
	}
	data, err := os.ReadFile(revisionPath(home, n))
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, n, fmt.Errorf("gateway settings revision %d: %w", n, ports.ErrNotFound)
	}
	if err != nil {
		return Settings{}, n, err
	}
	s, err := ParseDocument(data)
	if err == nil {
		err = validateSettings(s)
	}
	if err != nil {
		return s, n, fmt.Errorf("%s: %w", revisionPath(home, n), err)
	}
	return s, n, nil
}

// ReadSettings returns the settings the gateway runs on.
func ReadSettings(home string) (Settings, error) {
	s, _, err := RevisionSettings(home, 0)
	return s, err
}

// WriteSettings validates settings and stores them as a new revision (for setup and tests; the
// service applies through releases.Gateway).
func WriteSettings(home string, s Settings) error {
	_, err := WriteRevision(home, s, "", "settings")
	return err
}

// WriteRevision validates settings and stores them as the next revision, under the gateway lock;
// the next Sync applies them. Revisions beyond keepRevisions are pruned.
func WriteRevision(home string, s Settings, actor, source string) (int, error) {
	if err := validateSettings(s); err != nil {
		return 0, err
	}
	revs, err := Revisions(home)
	if err != nil {
		return 0, err
	}
	n := 1
	if len(revs) > 0 {
		n = revs[len(revs)-1].Revision + 1
	}
	if err := os.MkdirAll(settingsDir(home), 0o700); err != nil {
		return 0, err
	}
	if err := writeFile(revisionPath(home, n), Document(s)); err != nil {
		return 0, err
	}
	revs = append(revs, SettingsRevision{Revision: n, Time: time.Now().UTC().Truncate(time.Second), Actor: actor, Source: source})
	for len(revs) > keepRevisions {
		if err := os.Remove(revisionPath(home, revs[0].Revision)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		revs = revs[1:]
	}
	return n, writeHistory(home, revs)
}

// DropRevision removes revision n, which did not serve: the one before is in force again.
func DropRevision(home string, n int) error {
	revs, err := Revisions(home)
	if err != nil {
		return err
	}
	revs = slices.DeleteFunc(revs, func(r SettingsRevision) bool { return r.Revision == n })
	if err := writeHistory(home, revs); err != nil {
		return err
	}
	if err := os.Remove(revisionPath(home, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeHistory(home string, revs []SettingsRevision) error {
	data, err := yaml.Marshal(revs)
	if err != nil {
		return err
	}
	return writeFile(historyPath(home), data)
}
