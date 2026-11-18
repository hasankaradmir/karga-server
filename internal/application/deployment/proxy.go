package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
)

// KindProxy is the derived resource for a release's Envoy proxy (ADR-011). It is not written in
// packages: karga adds it to the plan when the release has Services.
const KindProxy = api.KindProxy

// ProxyResource is the name of the release proxy in the plan and the applied state.
const ProxyResource = "proxy"

// ProxyName is the container name of a release's proxy.
func ProxyName(release string) string {
	return release + "-" + resource.ReservedPrefix + "proxy"
}

// BackendAlias is the network alias a workload container of a revision answers to; only the release
// proxy uses it. Workloads get no alias of their own name.
func BackendAlias(workload string, revision int) string {
	return fmt.Sprintf("%s%s-r%d", resource.ReservedPrefix, workload, revision)
}

// ProxyDir holds a release proxy's configuration and admin socket. It outlives revisions.
func ProxyDir(home, release string) string {
	return filepath.Join(home, "proxies", release)
}

// ProxySpec is the desired proxy container: what forces a recreate or a network reconnect when it
// changes. Listeners, backends and routes are not part of it; they only rewrite the proxy's files.
type ProxySpec struct {
	Image  string `json:"image"`
	Limits string `json:"limits"`
	// Networks maps each network to the service names the proxy answers to on it.
	Networks map[string][]string `json:"networks"`
	// Published lists, per service, the ports published for the gateway (container ports cannot
	// change on a running container).
	Published map[string][]int `json:"published,omitempty"`
}

// proxyCluster names the cluster of one service port.
func proxyCluster(service string, port int) string { return fmt.Sprintf("%s-p%d", service, port) }

// services is a release's Services and the workloads they send traffic to.
type services struct {
	svc map[string]*resource.ServiceSpec
	wl  map[string]*resource.WorkloadSpec // only workloads some service targets
}

// servicesOf collects the Services of rs. Every workload a Service targets is behind the release
// proxy: it answers to its backend alias only.
func servicesOf(rs []resource.Resource) services {
	all := map[string]*resource.WorkloadSpec{}
	out := services{svc: map[string]*resource.ServiceSpec{}, wl: map[string]*resource.WorkloadSpec{}}
	for _, r := range rs {
		switch s := r.Spec.(type) {
		case *resource.WorkloadSpec:
			all[r.Metadata.Name] = s
		case *resource.ServiceSpec:
			out.svc[r.Metadata.Name] = s
		}
	}
	for _, s := range out.svc {
		if w := all[s.Workload]; w != nil {
			out.wl[s.Workload] = w
		}
	}
	return out
}

func (s services) names() []string { return slices.Sorted(maps.Keys(s.svc)) }

// targeting returns the services that send traffic to workload.
func (s services) targeting(workload string) []string {
	var out []string
	for _, n := range s.names() {
		if s.svc[n].Workload == workload {
			out = append(out, n)
		}
	}
	return out
}

// target returns the container port service port p goes to.
func (s services) target(svc string, p resource.ServicePort) (int, bool) {
	w := s.wl[s.svc[svc].Workload]
	if w == nil {
		return 0, false
	}
	wp, ok := w.PortNamed(p.TargetPort)
	return wp.ContainerPort, ok
}

// published returns, per service, the ports exposed on the gateway.
func (s services) published() map[string][]int {
	out := map[string][]int{}
	for _, n := range s.names() {
		for _, e := range s.svc[n].Expose {
			if p, ok := s.svc[n].PortNamed(e.Port); ok && !slices.Contains(out[n], p.Port) {
				out[n] = append(out[n], p.Port)
			}
		}
		slices.Sort(out[n])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func proxySpecFor(s services) ProxySpec {
	spec := ProxySpec{Image: envoy.Image(), Limits: envoyLimits, Networks: map[string][]string{}, Published: s.published()}
	for _, n := range s.names() {
		if w := s.wl[s.svc[n].Workload]; w != nil {
			for _, net := range w.Networks {
				spec.Networks[net] = append(spec.Networks[net], n)
			}
		}
	}
	return spec
}

func proxyApplied(releaseName string, spec ProxySpec) (release.AppliedResource, error) {
	js, err := json.Marshal(spec)
	if err != nil {
		return release.AppliedResource{}, err
	}
	sum := sha256.Sum256(js)
	return release.AppliedResource{Kind: KindProxy, Name: ProxyResource, RuntimeName: ProxyName(releaseName),
		Spec: js, Digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// appliedBehind returns the workloads the applied release proxy sends traffic to.
func appliedBehind(applied []release.AppliedResource) map[string]bool {
	out := map[string]bool{}
	for _, a := range applied {
		if a.Kind == resource.KindService {
			if r, err := resource.DecodeStored(a.Spec); err == nil {
				if s, ok := r.Spec.(*resource.ServiceSpec); ok {
					out[s.Workload] = true
				}
			}
		}
	}
	return out
}

// Proxy strategies: the container is recreated (image), or reconnected to networks whose aliases changed.
const (
	proxyRecreate  = "recreate"
	proxyReconnect = "reconnect"
)

// proxyChange plans the release proxy. Docker cannot change a running container's image or
// aliases: an image change recreates it, an alias change reconnects it to the affected networks.
// Both briefly interrupt the proxied connections of what they touch.
func proxyChange(releaseName string, svcs services, current map[Key]release.AppliedResource) (Change, error) {
	spec := proxySpecFor(svcs)
	want, err := proxyApplied(releaseName, spec)
	if err != nil {
		return Change{}, err
	}
	c := Change{Kind: KindProxy, Name: ProxyResource, Spec: want.Spec, Digest: want.Digest, RuntimeName: want.RuntimeName, Proxy: &spec}
	have, exists := current[Key{KindProxy, ProxyResource}]
	switch {
	case !exists:
		c.Action = ActionCreate
	case have.Digest == want.Digest:
		c.Action = ActionUnchanged
	default:
		c.Action, c.Applied = ActionUpdate, &have
		c.Fields = fieldChanges(KindProxy, have.Spec, want.Spec)
		var old ProxySpec
		_ = json.Unmarshal(have.Spec, &old)
		if old.Image != spec.Image || old.Limits != spec.Limits || !reflect.DeepEqual(old.Published, spec.Published) {
			c.Strategy, c.StrategyReason = proxyRecreate, "image, limits or exposed ports changed: brief interruption"
		} else {
			c.Strategy, c.StrategyReason = proxyReconnect, strings.Join(changedNetworks(old, spec), ", ")+": brief interruption"
		}
	}
	return c, nil
}

// changedNetworks lists the networks whose proxy aliases differ between two specs.
func changedNetworks(a, b ProxySpec) []string {
	var out []string
	for n := range a.Networks {
		if !slices.Equal(a.Networks[n], b.Networks[n]) {
			out = append(out, n)
		}
	}
	for n := range b.Networks {
		if _, ok := a.Networks[n]; !ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
