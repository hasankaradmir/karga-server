package deployment

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"
)

// How stored revisions and resource specs differ, field by field.

// fieldChanges lists leaf differences between two resource JSON documents. Lists compare as a whole.
func fieldChanges(kind string, oldJS, newJS json.RawMessage) []FieldChange {
	var o, n any
	_ = json.Unmarshal(oldJS, &o)
	_ = json.Unmarshal(newJS, &n)
	flatOld, flatNew := map[string]string{}, map[string]string{}
	flatten("", o, flatOld)
	flatten("", n, flatNew)
	paths := map[string]bool{}
	for p := range flatOld {
		paths[p] = true
	}
	for p := range flatNew {
		paths[p] = true
	}
	var out []FieldChange
	for p := range paths {
		if flatOld[p] == flatNew[p] {
			continue
		}
		fc := FieldChange{Path: p, Old: flatOld[p], New: flatNew[p]}
		if kind == resource.KindSecret && strings.HasPrefix(p, "spec.values.") {
			fc.Old, fc.New = "", "(changed)"
			if strings.HasPrefix(flatOld[p], `"sha256:`) && strings.HasPrefix(flatNew[p], `"hmac-sha256:`) {
				fc.New = "(digest scheme upgraded)"
			}
		}
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func flatten(prefix string, v any, out map[string]string) {
	if m, ok := v.(map[string]any); ok {
		for k, c := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, c, out)
		}
		return
	}
	js, _ := json.Marshal(v)
	out[prefix] = string(js)
}

// CompareManifests diffs two stored manifests (JSON arrays of resources, secret values already
// digests). Unchanged resources are left out; Secret values show as "(changed)".
func CompareManifests(from, to json.RawMessage) ([]ResourceDiff, error) {
	a, err := manifestDocs(from)
	if err != nil {
		return nil, err
	}
	b, err := manifestDocs(to)
	if err != nil {
		return nil, err
	}
	var out []ResourceDiff
	for k, doc := range b {
		old, ok := a[k]
		switch {
		case !ok:
			out = append(out, ResourceDiff{Kind: k.Kind, Name: k.Name, Action: ActionCreate})
		default:
			if fields := fieldChanges(k.Kind, old, doc); len(fields) > 0 {
				out = append(out, ResourceDiff{Kind: k.Kind, Name: k.Name, Action: ActionUpdate, Fields: fields})
			}
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, ResourceDiff{Kind: k.Kind, Name: k.Name, Action: ActionDelete})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if kindRank[out[i].Kind] != kindRank[out[j].Kind] {
			return kindRank[out[i].Kind] < kindRank[out[j].Kind]
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// CompareValues diffs two values documents field by field.
func CompareValues(from, to map[string]any) []FieldChange {
	a, _ := json.Marshal(from)
	b, _ := json.Marshal(to)
	return fieldChanges("", a, b)
}

func manifestDocs(js json.RawMessage) (map[Key]json.RawMessage, error) {
	var docs []json.RawMessage
	if len(js) > 0 {
		if err := json.Unmarshal(js, &docs); err != nil {
			return nil, err
		}
	}
	out := map[Key]json.RawMessage{}
	for _, d := range docs {
		var head struct {
			Kind     string            `json:"kind"`
			Metadata resource.Metadata `json:"metadata"`
		}
		if err := json.Unmarshal(d, &head); err != nil {
			return nil, fmt.Errorf("stored manifest: %w", err)
		}
		out[Key{head.Kind, head.Metadata.Name}] = d
	}
	return out, nil
}
