package filestore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

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
	if err := checkRelease(rev.Release); err != nil {
		return err
	}
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
	if err := checkRelease(rel); err != nil {
		return err
	}
	var f revisionFile
	if err := s.readYAML(revisionPath(rel, number), &f); err != nil {
		return notFound(err, fmt.Sprintf("revision %s/%d", rel, number))
	}
	f.Status, f.Description = string(status), description
	return s.write(revisionPath(rel, number), f)
}

// GetRevision implements ports.StateStore.
func (s *Store) GetRevision(_ context.Context, rel string, number int) (release.Revision, error) {
	if err := checkRelease(rel); err != nil {
		return release.Revision{}, err
	}
	var f revisionFile
	if err := s.readYAML(revisionPath(rel, number), &f); err != nil {
		return release.Revision{}, notFound(err, fmt.Sprintf("revision %d of release %s", number, rel))
	}
	return f.revision()
}

// ListRevisions implements ports.StateStore. Revisions come in ascending order.
func (s *Store) ListRevisions(ctx context.Context, rel string) ([]release.Revision, error) {
	if err := checkRelease(rel); err != nil {
		return nil, err
	}
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
	if err := checkRelease(rel); err != nil {
		return err
	}
	return s.remove(revisionPath(rel, number), false)
}
