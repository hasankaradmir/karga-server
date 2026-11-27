package filestore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

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
	if err := checkRelease(rel); err != nil {
		return nil, err
	}
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
	if err := checkRelease(rel); err != nil {
		return err
	}
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
	if err := checkRelease(rel); err != nil {
		return err
	}
	return s.remove(resourcePath(rel, kind, name), false)
}
