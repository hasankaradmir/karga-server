package releases

import (
	"strconv"

	"github.com/hasankaradmir/karga-sdk/packages"
)

// Stored values keep writeOnly values masked (packages.MaskValues); for --reuse-values these
// helpers find and drop the masked ones. A path step into a list is the element's index.

// redactedPaths lists the paths of m whose value is packages.Redacted, i.e. values masked when stored.
func redactedPaths(m map[string]any) [][]string {
	var out [][]string
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				walk(c, append(path, k))
			}
		case []any:
			for i, c := range t {
				walk(c, append(path, strconv.Itoa(i)))
			}
		case string:
			if t == packages.Redacted {
				out = append(out, append([]string(nil), path...))
			}
		}
	}
	walk(m, nil)
	return out
}

// dropValues returns a copy of m without the given paths. A value dropped from a list becomes nil,
// so the other elements keep their indexes.
func dropValues(m map[string]any, paths [][]string) map[string]any {
	out, _ := copyValues(m).(map[string]any)
	for _, p := range paths {
		if len(p) == 0 {
			continue
		}
		switch parent := at(out, p[:len(p)-1]).(type) {
		case map[string]any:
			delete(parent, p[len(p)-1])
		case []any:
			if i, err := strconv.Atoi(p[len(p)-1]); err == nil && i >= 0 && i < len(parent) {
				parent[i] = nil
			}
		}
	}
	return out
}

// hasValue reports whether m has a non-nil value at path.
func hasValue(m map[string]any, path []string) bool { return at(m, path) != nil }

// at returns the value of v at path (nil if there is none); a step into a list is an index.
func at(v any, path []string) any {
	for _, k := range path {
		switch t := v.(type) {
		case map[string]any:
			v = t[k]
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(t) {
				return nil
			}
			v = t[i]
		default:
			return nil
		}
	}
	return v
}

func copyValues(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, c := range t {
			m[k] = copyValues(c)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, c := range t {
			s[i] = copyValues(c)
		}
		return s
	default:
		return v
	}
}
