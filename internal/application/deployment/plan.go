package deployment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// Summary is what a plan does to one resource, as shown to the user.
type Summary struct {
	Kind   string
	Name   string
	Action string
	Fields []FieldChange
	// Strategy is how a workload update is applied (recreate or replace); StrategyReason says why
	// a requested replace fell back to recreate.
	Strategy       string
	StrategyReason string
	// Notice says what deleting a volume does to its data ("" for other changes).
	Notice string
}

// Label returns the strategy shown next to a change, e.g. "replace" or "recreate: mounts volume x read-write".
func (c Summary) Label() string {
	if c.StrategyReason != "" {
		return c.Strategy + ": " + c.StrategyReason
	}
	return c.Strategy
}

// FieldChange is one changed field, as JSON values. Secret values show only that they changed.
type FieldChange struct {
	Path, Old, New string
}

// ResourceDiff is how one resource differs between two revisions.
type ResourceDiff struct {
	Kind, Name string
	Action     string // create, update or delete (from the first to the second)
	Fields     []FieldChange
}

// Change is what a plan does to one resource: its summary, and what the executor needs to apply it.
type Change struct {
	Summary
	// Resource is the desired resource (nil for delete).
	Resource *resource.Resource `json:"-"`
	// Spec and Digest describe the desired state to record once applied.
	Spec   json.RawMessage `json:"-"`
	Digest string          `json:"-"`
	// Applied is the current state (nil for create).
	Applied *release.AppliedResource `json:"-"`
	// RuntimeName is the runtime object the change creates or replaces.
	RuntimeName string `json:"-"`
	// Proxy is the desired proxy of a KindProxy change (nil for delete).
	Proxy *ProxySpec `json:"-"`
}

// Plan is the ordered set of changes from the applied state to a manifest.
type Plan struct {
	Changes []Change
}

// Summaries are the plan's changes as shown to the user.
func (p Plan) Summaries() []Summary {
	out := make([]Summary, len(p.Changes))
	for i, c := range p.Changes {
		out[i] = c.Summary
	}
	return out
}

// HasChanges reports whether applying the plan would touch the runtime.
func (p Plan) HasChanges() bool {
	for _, c := range p.Changes {
		if c.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// Actions in a plan.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionDelete    = "delete"
	ActionUnchanged = "unchanged"
)

// containers names the containers workload may run as while the plan runs: the one the plan gives
// it, then the one it ran as before (a recreate removes that one first, a replace last).
func (p Plan) containers(workload string) []string {
	var out []string
	for _, c := range p.Changes {
		if c.Kind != resource.KindWorkload || c.Name != workload {
			continue
		}
		if c.Action != ActionDelete {
			out = append(out, c.RuntimeName)
		}
		if c.Applied != nil && c.Applied.RuntimeName != c.RuntimeName {
			out = append(out, c.Applied.RuntimeName)
		}
	}
	return out
}

// Digest is "hmac-sha256:<hex>" of data under the digest key, or "sha256:<hex>" without one (tests;
// karga-server always sets the key from $KARGA_HOME/state.key before it plans).
func (e *Env) Digest(data []byte) string {
	if len(e.key) > 0 {
		m := hmac.New(sha256.New, e.key)
		m.Write(data)
		return "hmac-sha256:" + hex.EncodeToString(m.Sum(nil))
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// redactedSecret marks secret values in stored specs; only a keyed digest of the value is kept.
func (e *Env) redactedSecret(v string) string { return e.Digest([]byte(v)) }

// Redacted returns r with secret values replaced by digests, for storage and display.
func (e *Env) Redacted(r resource.Resource) resource.Resource {
	s, ok := r.Spec.(*resource.SecretSpec)
	if !ok {
		return r
	}
	masked := &resource.SecretSpec{Values: map[string]string{}}
	for k, v := range s.Values {
		masked.Values[k] = e.redactedSecret(v)
	}
	r.Spec = masked
	return r
}

// withoutStrategy drops updateStrategy and dependsOn: they decide how and when a change is applied,
// not what runs.
func withoutStrategy(r resource.Resource) resource.Resource {
	if w, ok := r.Spec.(*resource.WorkloadSpec); ok && (w.UpdateStrategy != nil || w.DependsOn != nil) {
		c := *w
		c.UpdateStrategy, c.DependsOn = nil, nil
		r.Spec = &c
	}
	return r
}

// specs returns the canonical redacted JSON and digest of every resource. A workload's digest also
// covers the Config and Secret resources it mounts or reads variables from, and a Service's the
// Secrets it serves TLS with, so changing their content applies them again.
func (e *Env) specs(rs []resource.Resource) (map[Key]release.AppliedResource, error) {
	out := map[Key]release.AppliedResource{}
	behind := servicesOf(rs).wl
	pass := func(r resource.Resource) int {
		switch r.Spec.(type) {
		case *resource.ServiceSpec:
			return 1
		case *resource.WorkloadSpec:
			return 2
		}
		return 0
	}
	for p := range 3 { // Configs and Secrets first: the others fold their digests in
		for _, r := range rs {
			if pass(r) != p {
				continue
			}
			w, isWorkload := r.Spec.(*resource.WorkloadSpec)
			js, err := json.Marshal(withoutStrategy(e.Redacted(r)))
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			h.Write(js)
			if isWorkload {
				if behind[r.Metadata.Name] != nil {
					h.Write([]byte("\nproxied")) // behind the proxy the container answers to its backend alias
				}
				for _, m := range w.Mounts {
					kind, name := m.Target()
					if kind != resource.KindVolume {
						_, _ = fmt.Fprintf(h, "\n%s/%s=%s", kind, name, out[Key{kind, name}].Digest) // hash writes never fail
					}
				}
				// The container's environment holds the values: a new value needs a new container.
				for _, e := range w.EnvironmentFrom {
					_, _ = fmt.Fprintf(h, "\nenv/%s=%s", e.Secret, out[Key{resource.KindSecret, e.Secret}].Digest)
				}
			}
			if s, ok := r.Spec.(*resource.ServiceSpec); ok {
				for _, name := range s.TLSSecrets() {
					_, _ = fmt.Fprintf(h, "\ntls/%s=%s", name, out[Key{resource.KindSecret, name}].Digest) // a new certificate updates the gateway
				}
			}
			out[keyOf(r)] = release.AppliedResource{
				Kind: r.Kind, Name: r.Metadata.Name, Spec: js, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)),
			}
		}
	}
	return out, nil
}

// Anonymous are, per workload name, the image VOLUME paths its current container keeps in anonymous
// volumes (no Volume is mounted there). They are found in the runtime before planning.
type Anonymous map[string][]string

// Diff compares the desired manifest with what is applied. Changing a Network or Volume is refused:
// it would mean recreating it, which breaks attached workloads or destroys data. strategy overrides
// the workloads' updateStrategy when not empty.
func (e *Env) Diff(releaseName string, desired []resource.Resource, applied []release.AppliedResource, strategy string, anon Anonymous) (Plan, error) {
	if strategy != "" && strategy != resource.StrategyRecreate && strategy != resource.StrategyReplace {
		return Plan{}, fmt.Errorf("unknown strategy %q (want recreate or replace)", strategy)
	}
	specs, err := e.specs(desired)
	if err != nil {
		return Plan{}, err
	}
	current := map[Key]release.AppliedResource{}
	for _, a := range applied {
		current[Key{a.Kind, a.Name}] = a
	}
	hadProxy := appliedBehind(applied)
	svcs := servicesOf(desired)
	behind := svcs.wl
	ordered, err := Order(desired)
	if err != nil {
		return Plan{}, err
	}

	var plan Plan
	var errs []string
	if !e.host.Supported() && len(svcs.svc) > 0 {
		errs = append(errs, fmt.Sprintf("services %s need karga's gateway and release proxy (Linux with a local, rootful Docker)", strings.Join(svcs.names(), ", ")))
	}
	for _, r := range ordered {
		w, ok := r.Spec.(*resource.WorkloadSpec)
		if !ok {
			continue
		}
		if why := e.probeReachable(w); why != "" {
			errs = append(errs, fmt.Sprintf("workload %q: %s", r.Metadata.Name, why))
		}
	}
	for _, r := range ordered {
		r := r
		k := keyOf(r)
		want := specs[k]
		c := Change{Summary: Summary{Kind: k.Kind, Name: k.Name}, Resource: &r, Spec: want.Spec, Digest: want.Digest, RuntimeName: proxyops.RuntimeName(releaseName, k.Name)}
		if k.Kind == resource.KindService {
			c.RuntimeName = ProxyName(releaseName) // a Service has no container; the release proxy serves it
		}
		have, exists := current[k]
		switch {
		case !exists:
			c.Action = ActionCreate
		case have.Digest == want.Digest:
			c.Action = ActionUnchanged
		default:
			c.Action, c.Applied = ActionUpdate, &have
			c.Fields = fieldChanges(k.Kind, have.Spec, want.Spec)
			w, isWorkload := r.Spec.(*resource.WorkloadSpec)
			if isWorkload {
				c.Strategy, c.StrategyReason = workloadStrategy(w, anon[k.Name], strategy, behind[k.Name] != nil, hadProxy[k.Name])
			}
			if len(c.Fields) == 0 {
				why := "(mounted Config/Secret content or previous failure)"
				if isWorkload && (behind[k.Name] != nil) != hadProxy[k.Name] {
					why = "(service placement)"
				}
				c.Fields = []FieldChange{{Path: why}}
			}
			if k.Kind == resource.KindNetwork || k.Kind == resource.KindVolume {
				errs = append(errs, fmt.Sprintf("changing %s %q is not supported (uninstall the release or rename the resource)", k.Kind, k.Name))
			}
		}
		if k.Kind == resource.KindWorkload {
			// The container's name ends with its digest: an unchanged workload keeps the one it runs as.
			c.RuntimeName = proxyops.WorkloadName(releaseName, k.Name, want.Digest)
			if c.Action == ActionUnchanged {
				c.RuntimeName = have.RuntimeName
			}
		}
		plan.Changes = append(plan.Changes, c)
		delete(current, k)
	}
	if len(svcs.svc) > 0 {
		c, err := e.proxyChange(releaseName, svcs, current)
		if err != nil {
			return Plan{}, err
		}
		plan.Changes = append(plan.Changes, c)
		delete(current, Key{KindProxy, ProxyResource})
	}
	var removed []release.AppliedResource
	for _, a := range current {
		removed = append(removed, a)
	}
	sort.Slice(removed, func(i, j int) bool {
		if kindRank[removed[i].Kind] != kindRank[removed[j].Kind] {
			return kindRank[removed[i].Kind] > kindRank[removed[j].Kind] // workloads first
		}
		return removed[i].Name < removed[j].Name
	})
	for _, a := range removed {
		a := a
		c := Change{Summary: Summary{Kind: a.Kind, Name: a.Name, Action: ActionDelete}, Applied: &a, RuntimeName: a.RuntimeName}
		c.Notice = c.note()
		plan.Changes = append(plan.Changes, c)
	}
	if len(errs) > 0 {
		return plan, fmt.Errorf("release %s: %s", releaseName, strings.Join(errs, "; "))
	}
	return plan, nil
}

// workloadStrategy adds the service transitions to strategyFor: a workload a Service starts or
// stops targeting gains or loses its backend alias, which only a recreate can do.
func workloadStrategy(w *resource.WorkloadSpec, anon []string, override string, now, had bool) (string, string) {
	switch {
	case now && !had:
		return resource.StrategyRecreate, "a service now targets it"
	case !now && had:
		return resource.StrategyRecreate, "no service targets it anymore"
	}
	return strategyFor(w, anon, override)
}

// strategyFor decides how a workload update is applied. replace runs the new container next to the
// old one, which is only safe without read-write volumes (two writers on the same data) and with a
// healthcheck (to know it is ready).
func strategyFor(w *resource.WorkloadSpec, anon []string, override string) (string, string) {
	requested, _ := w.Strategy()
	if override != "" {
		requested = override
	}
	if requested != resource.StrategyReplace {
		return resource.StrategyRecreate, ""
	}
	return eligible(w, anon)
}

// eligible checks whether w can be replaced, ignoring what was requested.
func eligible(w *resource.WorkloadSpec, anon []string) (string, string) {
	for _, m := range w.Mounts {
		if m.Volume != "" && !m.ReadOnly {
			return resource.StrategyRecreate, fmt.Sprintf("mounts volume %s read-write", m.Volume)
		}
	}
	if len(anon) > 0 {
		return resource.StrategyRecreate, fmt.Sprintf("keeps image VOLUME %s in an anonymous volume", strings.Join(anon, ", "))
	}
	if w.HealthCheck == nil {
		return resource.StrategyRecreate, "no healthCheck to verify readiness"
	}
	return resource.StrategyReplace, ""
}

// DeletedVolumes lists the volumes the plan deletes with their data (retention delete).
func (p Plan) DeletedVolumes() []string {
	var out []string
	for _, c := range p.Changes {
		if c.deletesVolume() {
			out = append(out, c.Name)
		}
	}
	return out
}

func (c Change) deletesVolume() bool {
	return c.Action == ActionDelete && c.Kind == resource.KindVolume && c.Applied != nil && volumeRetention(c.Applied.Spec) == "delete"
}

// note explains what deleting a volume does to its data ("" for other changes).
func (c Change) note() string {
	switch {
	case c.Action != ActionDelete || c.Kind != resource.KindVolume:
		return ""
	case c.deletesVolume():
		return "its data is deleted"
	default:
		return "kept in the runtime: retention retain"
	}
}
