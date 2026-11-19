package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// KindProxy is the derived resource for a release's Envoy proxy (ADR-0011). It is not written in
// packages: karga adds it to the plan when the release has Services.
const KindProxy = "Proxy"

// ProxyResource is the name of the release proxy in the plan and the applied state.
const ProxyResource = "proxy"

// ProxyBase is what a release proxy's container name starts with; a Service, which has no
// container of its own, records it as its runtime name.
func ProxyBase(release string) string {
	return release + "-" + resource.ReservedPrefix + "proxy"
}

// ProxyName is the container name of a release proxy of spec digest: like a workload's (D34), it
// ends with 8 hex of the digest, so a new proxy can start next to the one it replaces.
func ProxyName(release, digest string) string {
	return proxyops.WorkloadName(release, resource.ReservedPrefix+"proxy", digest)
}

// ProxyAdminDir is the directory holding the admin socket of proxy container name: its instance
// directory below ProxyDir (a container named ProxyBase, from before D37, uses ProxyDir itself).
func ProxyAdminDir(home, release, name string) string {
	if inst, ok := strings.CutPrefix(name, ProxyBase(release)+"-"); ok {
		return filepath.Join(ProxyDir(home, release), inst)
	}
	return ProxyDir(home, release)
}

// proxyInstance is the instance (WriteInstance) of proxy container name, "" for ProxyDir's own.
func proxyInstance(release, name string) string {
	inst, _ := strings.CutPrefix(name, ProxyBase(release)+"-")
	if inst == name {
		return ""
	}
	return inst
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
	wp, ok := w.TargetPort(p.TargetPort)
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

func (e *Env) proxySpecFor(s services) ProxySpec {
	spec := ProxySpec{Image: e.host.Proxies.Container().Image, Limits: e.host.Proxies.Container().Source, Networks: map[string][]string{}, Published: s.published()}
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
	digest := "sha256:" + hex.EncodeToString(sum[:])
	return release.AppliedResource{Kind: KindProxy, Name: ProxyResource, RuntimeName: ProxyName(releaseName, digest),
		Spec: js, Digest: digest}, nil
}

// Proxy strategies: a new proxy starts next to the old one and takes over (replace), or, when the
// new name is the old one's, the old one goes first (recreate).
const (
	proxyReplace  = "replace"
	proxyRecreate = "recreate"
)

// proxyChange plans the release proxy. Docker cannot change a running container's image, ports
// or aliases, so a changed proxy is a new container: it starts next to the old one, the gateway
// switches to it, and the old one drains and goes.
func (e *Env) proxyChange(releaseName string, svcs services, current map[Key]release.AppliedResource) (Change, error) {
	spec := e.proxySpecFor(svcs)
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
		c.Action, c.RuntimeName = ActionUnchanged, have.RuntimeName
	default:
		c.Action, c.Applied = ActionUpdate, &have
		c.Fields = fieldChanges(KindProxy, have.Spec, want.Spec)
		c.Strategy = proxyReplace
		if c.RuntimeName == have.RuntimeName {
			// shortcut: 8 hex can collide (1 in 4 billion per change); the old proxy then goes first.
			c.Strategy, c.StrategyReason = proxyRecreate, "the new proxy's name is the old one's: brief interruption"
		}
	}
	return c, nil
}
