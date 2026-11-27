package filestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

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
	if err := checkRelease(name); err != nil {
		return release.Release{}, err
	}
	var f releaseFile
	if err := s.readYAML(releaseDir(name)+"/release.yaml", &f); err != nil {
		return release.Release{}, notFound(err, "release "+name)
	}
	// A file cut short (a disk error, a hand edit) parses, with fields left empty.
	if f.Name != name || f.Status == "" {
		return release.Release{}, fmt.Errorf("%s is incomplete (name %q, status %q)", s.path(releaseDir(name)+"/release.yaml"), f.Name, f.Status)
	}
	return f.release(), nil
}

// ListReleases implements ports.StateStore. Releases come sorted by name. One whose file cannot be
// read is listed as unreadable (GetRelease says why), so that it does not stop the others.
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
			r = release.Release{Name: n, Status: release.StatusUnreadable}
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
	if err := checkRelease(r.Name); err != nil {
		return err
	}
	if old, err := s.GetRelease(ctx, r.Name); err == nil {
		r.CreatedAt = old.CreatedAt
	}
	return s.write(releaseDir(r.Name)+"/release.yaml", releaseFile{Name: r.Name, PackageName: r.PackageName, PackageVersion: r.PackageVersion,
		CurrentRevision: r.CurrentRevision, Status: string(r.Status), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Paused: r.Paused})
}

// DeleteRelease implements ports.StateStore: the release with its revisions and resources. Its
// operations stay, like an audit log.
func (s *Store) DeleteRelease(_ context.Context, name string) error {
	if err := checkRelease(name); err != nil {
		return err
	}
	return s.remove(releaseDir(name), true)
}
