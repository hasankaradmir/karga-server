package deployment

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"
)

// kindRank breaks ties between independent resources so the order is stable.
var kindRank = map[string]int{
	resource.KindNetwork: 0, resource.KindVolume: 1, resource.KindConfig: 2, resource.KindSecret: 3, resource.KindWorkload: 4,
	resource.KindService: 5, KindProxy: 6,
}

// dependencies returns the resources r needs to exist first.
func dependencies(r resource.Resource) []Key {
	if s, ok := r.Spec.(*resource.ServiceSpec); ok {
		deps := []Key{{resource.KindWorkload, s.Workload}}
		for _, name := range s.TLSSecrets() {
			deps = append(deps, Key{resource.KindSecret, name})
		}
		return deps
	}
	w, ok := r.Spec.(*resource.WorkloadSpec)
	if !ok {
		return nil
	}
	var deps []Key
	for _, n := range w.Networks {
		deps = append(deps, Key{resource.KindNetwork, n})
	}
	for _, m := range w.Mounts {
		kind, name := m.Target()
		deps = append(deps, Key{kind, name})
	}
	for _, s := range w.DependsOn {
		deps = append(deps, Key{resource.KindService, s}) // which depends on its workload
	}
	return deps
}

// Order returns rs in creation order (dependencies first). Delete uses the reverse.
func Order(rs []resource.Resource) ([]resource.Resource, error) {
	return order(rs, dependencies)
}

// reverse returns a reversed copy of s.
func reverse[T any](s []T) []T {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

func order(rs []resource.Resource, deps func(resource.Resource) []Key) ([]resource.Resource, error) {
	byKey := map[Key]resource.Resource{}
	for _, r := range rs {
		byKey[keyOf(r)] = r
	}
	indegree := map[Key]int{}
	dependents := map[Key][]Key{}
	for _, r := range rs {
		k := keyOf(r)
		indegree[k] += 0
		for _, d := range deps(r) {
			if _, ok := byKey[d]; !ok {
				return nil, fmt.Errorf("%s %q depends on undefined %s %q", k.Kind, k.Name, d.Kind, d.Name)
			}
			indegree[k]++
			dependents[d] = append(dependents[d], k)
		}
	}

	less := func(a, b Key) bool {
		if kindRank[a.Kind] != kindRank[b.Kind] {
			return kindRank[a.Kind] < kindRank[b.Kind]
		}
		return a.Name < b.Name
	}
	var ready []Key
	for k, n := range indegree {
		if n == 0 {
			ready = append(ready, k)
		}
	}
	var out []resource.Resource
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		k := ready[0]
		ready = ready[1:]
		out = append(out, byKey[k])
		for _, d := range dependents[k] {
			indegree[d]--
			if indegree[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(out) < len(byKey) {
		var cyc []string
		for k, n := range indegree {
			if n > 0 {
				cyc = append(cyc, k.Kind+"/"+k.Name)
			}
		}
		sort.Strings(cyc)
		return nil, fmt.Errorf("circular dependency between %s", strings.Join(cyc, ", "))
	}
	return out, nil
}
