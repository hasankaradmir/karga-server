package releases

import "github.com/hasankaradmir/karga-sdk/packages"

// Stored values keep writeOnly values masked; these helpers mask them and, for --reuse-values,
// find and drop the masked ones.

// maskValues returns a copy of m with every present, non-nil value at paths replaced by packages.Redacted.
func maskValues(m map[string]any, paths [][]string) map[string]any {
	out, _ := copyValues(m).(map[string]any)
	for _, p := range paths {
		cur := out
		for i, k := range p {
			if i == len(p)-1 {
				if v, ok := cur[k]; ok && v != nil {
					cur[k] = packages.Redacted
				}
				break
			}
			next, ok := cur[k].(map[string]any)
			if !ok {
				break
			}
			cur = next
		}
	}
	return out
}

// redactedPaths lists the paths of m whose value is packages.Redacted, i.e. values maskValues removed.
func redactedPaths(m map[string]any) [][]string {
	var out [][]string
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				walk(c, append(path, k))
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

// dropValues returns a copy of m without the given paths.
func dropValues(m map[string]any, paths [][]string) map[string]any {
	out, _ := copyValues(m).(map[string]any)
	for _, p := range paths {
		cur := out
		for i, k := range p {
			if i == len(p)-1 {
				delete(cur, k)
				break
			}
			next, ok := cur[k].(map[string]any)
			if !ok {
				break
			}
			cur = next
		}
	}
	return out
}

// hasValue reports whether m has a non-nil value at path.
func hasValue(m map[string]any, path []string) bool {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = mm[k]; !ok {
			return false
		}
	}
	return cur != nil
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
