package deployment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-sdk/api"

	"encoding/json"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// DefaultTimeout bounds how long Execute waits for workloads to become ready.
const DefaultTimeout = 5 * time.Minute

// Step statuses reported to ExecRequest.OnStep. StepKept means the change failed before anything
// running was touched: the previous state is still applied (a replace whose new container failed).
const (
	StepDone   = "done"
	StepFailed = "failed"
	StepKept   = "kept-previous"
)

// ExecError is returned by Execute. Disrupted is false when the failure happened before any running
// workload was stopped, removed or created: what ran before still runs, unchanged.
type ExecError struct {
	Err       error
	Disrupted bool
}

func (e *ExecError) Error() string { return e.Err.Error() }
func (e *ExecError) Unwrap() error { return e.Err }

// nextSuffix names the container a replace starts next to the current one.
const nextSuffix = "-next"

// ExecRequest applies Plan, computed from Resources against the applied state, as revision Revision.
type ExecRequest struct {
	Home      string
	Release   string
	Revision  int
	Supported []string
	// Resources is the full desired manifest (pre-flight and materialization need all of it).
	Resources []resource.Resource
	Plan      Plan
	// Redact receives every secret value before anything is logged.
	Redact  func(string)
	Timeout time.Duration
	// OnStep is called after every runtime step so the caller can record applied state and progress.
	OnStep func(Change, release.Step) error
	// OnProgress, if set, receives what is going on as it happens (for display only).
	OnProgress func(Event)
	// Proxies controls the release proxy and the gateway (default: Envoy). ProxyUser is the uid:gid
	// the release proxy runs as (default: the karga process user, so it can use its 0700 directory).
	Proxies   ProxyControl
	ProxyUser string
	// AllowedCaps are the capabilities beyond resource.DefaultCaps workloads may add.
	AllowedCaps []string
}

func labels(release string, revision int, kind, name string) map[string]string {
	return map[string]string{
		ports.LabelManaged:  "true",
		ports.LabelRelease:  release,
		ports.LabelKind:     kind,
		ports.LabelName:     name,
		ports.LabelRevision: strconv.Itoa(revision),
	}
}

// Execute applies a plan with the recreate strategy (D2):
//  1. pre-flight, materialize files, and ensure every needed image before touching anything running;
//  2. delete removed workloads (frees their ports);
//  3. create and update in dependency order. A recreate update is stop, delete, create, start; a
//     replace update starts the new container next to the old one and switches over (replaceStart);
//  4. wait for recreated workloads; ones that do not become ready are stopped, not removed (D14);
//  5. delete removed networks and volumes (volumes only with retention delete).
//
// Errors after the pre-flight phase are *ExecError.
func Execute(ctx context.Context, rt ports.Runtime, req ExecRequest) error {
	ctx = withProgress(ctx, req.OnProgress)
	disrupted := false
	err := execute(ctx, rt, req, &disrupted)
	if err == nil {
		return nil
	}
	if disrupted && proxySupported.Load() {
		// Point the gateway at what runs now; the error that got us here is the one to report.
		_, _ = gateway.Sync(ctx, rt, proxyControl(req), req.Home)
	}
	return &ExecError{Err: err, Disrupted: disrupted}
}

// Proxies controls the release proxies and the gateway when a request names none. Tests that run
// without Envoy replace it.
var Proxies ProxyControl = EnvoyProxies{}

func proxyControl(req ExecRequest) ProxyControl {
	if req.Proxies == nil {
		return Proxies
	}
	return req.Proxies
}

func execute(ctx context.Context, rt ports.Runtime, req ExecRequest, disrupted *bool) error {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.Redact == nil {
		req.Redact = func(string) {}
	}
	if req.OnStep == nil {
		req.OnStep = func(Change, release.Step) error { return nil }
	}
	req.Proxies = proxyControl(req)
	if req.ProxyUser == "" {
		req.ProxyUser = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	report := func(c Change, action string, err error) error {
		st := release.Step{Kind: c.Kind, Name: c.Name, Action: action, Status: StepDone}
		if err != nil {
			st.Status, st.Error = StepFailed, err.Error()
			progress(ctx, c.Kind, c.Name, PhaseFailed, "%s: %v", action, err)
		} else {
			progress(ctx, c.Kind, c.Name, PhaseDone, "%s", action)
		}
		if serr := req.OnStep(c, st); serr != nil {
			return errors.Join(err, serr)
		}
		if err != nil {
			return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
		}
		return nil
	}

	info, err := rt.Info(ctx)
	if err != nil {
		return err
	}
	if err := preflightLocked(ctx, rt, info, req); err != nil {
		return err
	}
	paths, err := Materialize(req.Home, req.Release, req.Revision, req.Resources, req.Redact)
	if err != nil {
		return err
	}
	svcs := servicesOf(req.Resources)
	needGateway := len(svcs.published()) > 0
	for _, c := range req.Plan.Changes {
		if c.Action != ActionCreate && c.Action != ActionUpdate {
			continue
		}
		if w, ok := workloadOf(c); ok {
			progress(ctx, c.Kind, c.Name, PhasePull, "image %s (%s)", w.Image, w.PullPolicy)
			if err := rt.EnsureImage(ctx, w.Image, w.PullPolicy); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
		if c.Kind == KindProxy {
			if err := rt.EnsureImage(ctx, c.Proxy.Image, "if-not-present"); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
	}
	if needGateway {
		if err := rt.EnsureImage(ctx, envoy.Image(), "if-not-present"); err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
	}
	owned, err := managedObjects(ctx, rt, req.Release)
	if err != nil {
		return err
	}

	for _, c := range req.Plan.Changes {
		if c.Action == ActionDelete && c.Kind == resource.KindWorkload {
			*disrupted = true
			if err := report(c, ActionDelete, removeWorkload(ctx, rt, c.Applied.RuntimeName)); err != nil {
				return err
			}
		}
	}

	// Proxies that go away or are recreated release the workloads' aliases first.
	for _, c := range req.Plan.Changes {
		if c.Kind == KindProxy && (c.Action == ActionDelete || c.Strategy == proxyRecreate) {
			*disrupted = true
			if err := report(c, ActionDelete, removeProxy(ctx, rt, req.Home, req.Release, c, c.Action == ActionDelete)); err != nil {
				return err
			}
		}
	}

	var changed []Change
	waited := 0 // changed[:waited] are ready
	waitPending := func() error {
		if waited == len(changed) {
			return nil
		}
		err := waitChanged(ctx, rt, req.Release, changed[waited:], req.Timeout, report)
		waited = len(changed)
		return err
	}
	// The proxy starts or changes once the workloads before it are ready, pointing at what runs now:
	// at the end, or before the first workload that depends on a Service.
	proxyDone, waitAll := false, false
	proxyUp := func() error {
		if proxyDone {
			return nil
		}
		proxyDone = true
		for _, c := range req.Plan.Changes {
			if c.Kind != KindProxy || (c.Action != ActionCreate && c.Action != ActionUpdate) {
				continue
			}
			*disrupted, waitAll = true, true
			var err error
			if c.Action == ActionCreate || c.Strategy == proxyRecreate {
				err = createProxy(ctx, rt, req.Proxies, req, c, svcs)
			} else {
				err = reconnectProxy(ctx, rt, req.Release, c)
			}
			if err := report(c, c.Action, err); err != nil {
				return err
			}
		}
		return nil
	}
	// dependencies waits until the Services a workload depends on are ready behind the proxy.
	dependencies := func(c Change, w *resource.WorkloadSpec) error {
		if len(w.DependsOn) == 0 {
			return nil
		}
		if err := waitPending(); err != nil {
			return err
		}
		if err := proxyUp(); err != nil {
			return err
		}
		if err := writeProxy(ctx, rt, req.Proxies, req, svcs, nil); err != nil {
			return err
		}
		progress(ctx, c.Kind, c.Name, PhaseWait, "waiting for %s", strings.Join(w.DependsOn, ", "))
		want := map[string]string{}
		for _, d := range w.DependsOn {
			if dep := svcs.svc[d]; dep != nil {
				rev, err := revisionOf(ctx, rt, RuntimeName(req.Release, dep.Workload))
				if err != nil {
					return fmt.Errorf("%s %q depends on service %s, whose workload %s does not run: %w", c.Kind, c.Name, d, dep.Workload, err)
				}
				maps.Copy(want, proxyWant(svcs, dep.Workload, BackendAlias(dep.Workload, rev)))
			}
		}
		return envoyErr(ctx, rt, ProxyName(req.Release), req.Proxies.WaitReady(ctx, ProxyDir(req.Home, req.Release), want, req.Timeout))
	}

	for _, c := range req.Plan.Changes {
		if c.Kind == KindProxy || (c.Action != ActionCreate && c.Action != ActionUpdate) {
			continue
		}
		if w, ok := workloadOf(c); ok {
			if err := dependencies(c, w); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
		name := RuntimeName(req.Release, c.Name)
		lbl := labels(req.Release, req.Revision, c.Kind, c.Name)
		var err error
		switch s := c.Resource.Spec.(type) {
		case *resource.NetworkSpec:
			err = ownedOK(owned, ports.ObjectNetwork, name,
				rt.CreateNetwork(ctx, ports.NetworkRequest{Name: name, Labels: lbl, Driver: s.Driver, Internal: s.Internal}))
		case *resource.VolumeSpec:
			lbl[ports.LabelRetention] = s.Retention
			err = ownedOK(owned, ports.ObjectVolume, name,
				rt.CreateVolume(ctx, ports.VolumeRequest{Name: name, Labels: lbl, Driver: s.Driver}))
		case *resource.WorkloadSpec:
			lbl[ports.LabelDigest] = c.Digest
			if c.Action == ActionUpdate && c.Strategy == resource.StrategyReplace {
				var touched bool
				var rerr error
				touched, rerr = replaceWorkload(ctx, rt, req, c, owned, s, lbl, paths, svcs)
				*disrupted = *disrupted || touched
				if rerr != nil && !touched {
					_ = req.OnStep(c, release.Step{Kind: c.Kind, Name: c.Name, Action: c.Action, Status: StepKept, Error: rerr.Error()})
					err := fmt.Errorf("%s %q: %w (the previous container is still running)", c.Kind, c.Name, rerr)
					// Workloads recreated earlier in this plan are waited for like on success: ones that do not
					// become ready are stopped and reported failed, so the next plan recreates them.
					err = errors.Join(err, waitPending())
					return err
				}
				err = rerr
				break
			}
			*disrupted = true
			err = recreateWorkload(ctx, rt, c, owned, req.Release, s, lbl, paths, svcs.wl[c.Name] != nil)
			changed = append(changed, c)
		}
		// Config and Secret have no runtime object: they were materialized above.
		if err := report(c, c.Action, err); err != nil {
			return err
		}
	}

	if err := waitPending(); err != nil {
		return err
	}
	if err := proxyUp(); err != nil {
		return err
	}
	if len(svcs.svc) > 0 {
		if err := writeProxy(ctx, rt, req.Proxies, req, svcs, nil); err != nil {
			return err
		}
		want := map[string]string{}
		for _, c := range req.Plan.Changes {
			changedService := c.Kind == resource.KindService && c.Action != ActionUnchanged && c.Action != ActionDelete
			if svcs.wl[c.Name] != nil && c.Kind == resource.KindWorkload && (waitAll || c.Action != ActionUnchanged) {
				if rev, err := revisionOf(ctx, rt, RuntimeName(req.Release, c.Name)); err == nil {
					maps.Copy(want, proxyWant(svcs, c.Name, BackendAlias(c.Name, rev)))
				}
			}
			if s := svcs.svc[c.Name]; changedService && s != nil {
				if rev, err := revisionOf(ctx, rt, RuntimeName(req.Release, s.Workload)); err == nil {
					maps.Copy(want, proxyWant(svcs, s.Workload, BackendAlias(s.Workload, rev)))
				}
			}
		}
		if len(want) > 0 {
			if err := envoyErr(ctx, rt, ProxyName(req.Release), req.Proxies.WaitReady(ctx, ProxyDir(req.Home, req.Release), want, proxyReadyTimeout)); err != nil {
				return err
			}
		}
	}
	if proxySupported.Load() {
		if err := gateway.WriteExpose(req.Home, req.Release, exposeRoutes(svcs, paths)); err != nil {
			return err
		}
		progress(ctx, "Gateway", gateway.ContainerName, PhaseSwitch, "updating the host gateway")
		targets, err := gateway.Sync(ctx, rt, req.Proxies, req.Home)
		if err == nil {
			err = envoyErr(ctx, rt, gateway.ContainerName, gateway.Wait(ctx, req.Proxies, req.Home, req.Release, targets))
		}
		if err != nil {
			*disrupted = true
			return fmt.Errorf("gateway: %w", err)
		}
	}

	for _, c := range Reverse(req.Plan.Changes) {
		if c.Action != ActionDelete || c.Kind == resource.KindWorkload || c.Kind == KindProxy {
			continue
		}
		var err error
		switch c.Kind {
		case resource.KindNetwork:
			*disrupted = true
			err = rt.DeleteNetwork(ctx, c.Applied.RuntimeName)
		case resource.KindVolume:
			if volumeRetention(c.Applied.Spec) == "delete" {
				*disrupted = true
				err = rt.DeleteVolume(ctx, c.Applied.RuntimeName)
			}
		}
		if err := report(c, ActionDelete, err); err != nil {
			return err
		}
	}
	return nil
}

// preflightLocked runs Preflight under the gateway lock: another karga process could otherwise
// bind a gateway port between this one listing the gateway's claims and test-binding the port.
func preflightLocked(ctx context.Context, rt ports.Runtime, info ports.RuntimeInfo, req ExecRequest) error {
	if proxySupported.Load() {
		unlock, err := gateway.Lock(req.Home)
		if err != nil {
			return err
		}
		defer unlock()
	}
	return Preflight(ctx, rt, info, req.Home, req.Release, req.Supported, req.Resources, req.AllowedCaps)
}

func workloadOf(c Change) (*resource.WorkloadSpec, bool) {
	if c.Resource == nil {
		return nil, false
	}
	w, ok := c.Resource.Spec.(*resource.WorkloadSpec)
	return w, ok
}

func volumeRetention(spec []byte) string {
	var v struct {
		Spec resource.VolumeSpec `json:"spec"`
	}
	_ = json.Unmarshal(spec, &v)
	return v.Spec.Retention
}

// managedObjects returns "kind/name" of every runtime object labelled as belonging to release.
func managedObjects(ctx context.Context, rt ports.Runtime, rel string) (map[string]bool, error) {
	objs, err := rt.ListManaged(ctx, rel)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range objs {
		out[o.Kind+"/"+o.Name] = true
	}
	return out, nil
}

// ownedOK accepts an existing network or volume only if it already belongs to this release
// (volumes are reused across reinstalls on purpose). Anything else with the name is an error.
func ownedOK(owned map[string]bool, kind, name string, err error) error {
	if !errors.Is(err, ports.ErrConflict) {
		return err
	}
	if owned[kind+"/"+name] {
		return nil
	}
	return fmt.Errorf("%s %s already exists and is not managed by this release: %w", kind, name, err)
}

// recreateWorkload recreates a workload: the old container goes first, so ports and names are free.
// Its anonymous volumes (image VOLUME paths no Volume covers) move to the new container, as docker
// compose does, so their data survives the update.
func recreateWorkload(ctx context.Context, rt ports.Runtime, c Change, owned map[string]bool, rel string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, behind bool) error {
	name := RuntimeName(rel, c.Name)
	wr, err := workloadRequest(rel, c.Name, s, lbl, paths, behind)
	if err != nil {
		return err
	}
	if c.Applied != nil {
		progress(ctx, c.Kind, c.Name, PhaseStop, "stopping and removing %s", c.Applied.RuntimeName)
		if wr.Mounts, err = takeOver(ctx, rt, c.Applied.RuntimeName, wr.Mounts); err != nil {
			return err
		}
	}
	progress(ctx, c.Kind, c.Name, PhaseCreate, "creating %s", name)
	err = rt.CreateWorkload(ctx, wr)
	if errors.Is(err, ports.ErrConflict) && owned[ports.ObjectWorkload+"/"+name] {
		// Left over from an interrupted operation of this release; it may hold the moved volumes.
		if wr.Mounts, err = takeOver(ctx, rt, name, wr.Mounts); err == nil {
			err = rt.CreateWorkload(ctx, wr)
		}
	}
	if err != nil {
		return err
	}
	return rt.StartWorkload(ctx, name)
}

// takeOver stops and deletes a container, keeping its anonymous volumes, and returns mounts with
// those volumes added at their paths (unless a mount already uses the path).
func takeOver(ctx context.Context, rt ports.Runtime, name string, mounts []ports.Mount) ([]ports.Mount, error) {
	st, err := rt.GetWorkload(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return mounts, nil
	}
	if err != nil {
		return mounts, err
	}
	if err := rt.StopWorkload(ctx, name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return mounts, err
	}
	if err := rt.DeleteWorkload(ctx, name); err != nil {
		return mounts, err
	}
	for _, a := range st.AnonymousVolumes {
		if !slices.ContainsFunc(mounts, func(m ports.Mount) bool { return m.Target == a.Target }) {
			mounts = append(mounts, ports.Mount{Type: ports.MountVolume, Source: a.Source, Target: a.Target})
		}
	}
	return mounts, nil
}

// replaceWorkload updates a workload without downtime (ADR-011): its clients reach it only
// through its Services, that is through the release proxy, so switching the proxy switches all of
// them, from the release's networks and through the gateway alike:
//  1. remove a leftover <name>-next of this release;
//  2. create <name>-next with its revision's backend alias (nobody resolves it yet), start it and
//     wait until healthy; on failure stop it (kept for inspection) and return untouched;
//  3. point the proxy at the new alias and wait until it routes there; on failure restore it;
//  4. wait until the proxy drained the old container (at most the drain timeout), stop it, remove
//     it and rename <name>-next to the canonical name.
//
// A workload no Service targets receives no traffic: it is started next to the old one and the old
// one is stopped once the new one is healthy.
//
// touched reports whether anything that was serving traffic has been changed.
func replaceWorkload(ctx context.Context, rt ports.Runtime, req ExecRequest, c Change, owned map[string]bool,
	s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, svcs services) (touched bool, err error) {
	name := RuntimeName(req.Release, c.Name)
	next := name + nextSuffix
	behind := svcs.wl[c.Name] != nil
	if owned[ports.ObjectWorkload+"/"+next] {
		if err := removeWorkload(ctx, rt, next); err != nil {
			return false, err
		}
	}
	wr, err := workloadRequest(req.Release, c.Name, s, lbl, paths, behind)
	if err != nil {
		return false, err
	}
	wr.Name = next
	progress(ctx, c.Kind, c.Name, PhaseCreate, "starting %s next to %s", next, c.Applied.RuntimeName)
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		return false, err
	}
	if err := rt.StartWorkload(ctx, next); err != nil {
		return false, err
	}
	stopNext := func(cause error) error {
		if serr := rt.StopWorkload(ctx, next, 0); serr != nil && !errors.Is(serr, ports.ErrNotFound) {
			return errors.Join(cause, serr)
		}
		return cause
	}
	if _, err := waitReady(ctx, rt, []readyTarget{{next, s}}, req.Timeout); err != nil {
		return false, stopNext(err)
	}

	_, drain := s.Strategy()
	if behind {
		progress(ctx, c.Kind, c.Name, PhaseSwitch, "switching traffic to %s", next)
		alias := BackendAlias(c.Name, req.Revision)
		if err := writeProxy(ctx, rt, req.Proxies, req, svcs, map[string]string{c.Name: alias}); err != nil {
			return false, stopNext(err)
		}
		if err := envoyErr(ctx, rt, ProxyName(req.Release), req.Proxies.WaitReady(ctx, ProxyDir(req.Home, req.Release), proxyWant(svcs, c.Name, alias), proxyReadyTimeout)); err != nil {
			// The proxy may have routed to the new backend for a moment: count it as touched.
			return true, stopNext(errors.Join(err, writeProxy(ctx, rt, req.Proxies, req, svcs, nil)))
		}
		progress(ctx, c.Kind, c.Name, PhaseDrain, "waiting up to %s for connections to %s to finish", drain, c.Applied.RuntimeName)
		if _, err := req.Proxies.WaitDrained(ctx, ProxyDir(req.Home, req.Release), proxyClusters(svcs, c.Name), drain); err != nil {
			return true, err
		}
	}
	old := c.Applied.RuntimeName
	progress(ctx, c.Kind, c.Name, PhaseStop, "stopping %s", old)
	if err := rt.StopWorkload(ctx, old, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return true, err
	}
	if err := deleteWorkload(ctx, rt, old); err != nil {
		return true, err
	}
	return true, rt.RenameWorkload(ctx, next, name)
}

// Reconcile repairs what an interrupted replace leaves behind, before the next plan is made:
// with both <name> and <name>-next present the one the proxies route to is kept (the old one
// unless they had switched); with only <name>-next it takes the canonical name and is returned with
// an empty digest so the plan applies the workload again.
func Reconcile(ctx context.Context, rt ports.Runtime, home, rel string, applied []release.AppliedResource) ([]release.AppliedResource, error) {
	owned, err := managedObjects(ctx, rt, rel)
	if err != nil {
		return nil, err
	}
	var repaired []release.AppliedResource
	for _, a := range applied {
		if a.Kind != resource.KindWorkload || !owned[ports.ObjectWorkload+"/"+a.RuntimeName+nextSuffix] {
			continue
		}
		next := a.RuntimeName + nextSuffix
		if owned[ports.ObjectWorkload+"/"+a.RuntimeName] && routedTo(ctx, rt, home, rel, a.Name, next) {
			if err := removeWorkload(ctx, rt, a.RuntimeName); err != nil {
				return nil, err
			}
			delete(owned, ports.ObjectWorkload+"/"+a.RuntimeName)
		}
		if owned[ports.ObjectWorkload+"/"+a.RuntimeName] {
			if err := removeWorkload(ctx, rt, next); err != nil {
				return nil, err
			}
			continue
		}
		if err := rt.RenameWorkload(ctx, next, a.RuntimeName); err != nil {
			return nil, err
		}
		a.Digest = ""
		repaired = append(repaired, a)
	}
	return repaired, nil
}

// waitChanged waits for every changed workload. On failure the ones that are not ready are stopped
// (kept for inspection) and reported as failed, so the next plan recreates them.
func waitChanged(ctx context.Context, rt ports.Runtime, rel string, changed []Change, timeout time.Duration, report func(Change, string, error) error) error {
	targets := make([]readyTarget, len(changed))
	for i, c := range changed {
		w, _ := workloadOf(c)
		targets[i] = readyTarget{RuntimeName(rel, c.Name), w}
	}
	notReady, werr := waitReady(ctx, rt, targets, timeout)
	if werr == nil {
		return nil
	}
	errs := []error{werr}
	for _, c := range changed {
		name := RuntimeName(rel, c.Name)
		st, err := rt.GetWorkload(ctx, name)
		if err == nil && !slices.Contains(notReady, name) && st.State == ports.StateRunning && st.Health != ports.HealthUnhealthy && st.Health != ports.HealthStarting {
			continue
		}
		if err := rt.StopWorkload(ctx, name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
			errs = append(errs, err)
		}
		_ = report(c, "wait", werr)
	}
	return errors.Join(errs...)
}

// workloadRequest builds the container of a workload. It publishes no port and gets no alias of its
// name: behind the release proxy (a Service targets it) it answers to its revision's backend alias,
// meant for the proxy.
func workloadRequest(release, name string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, behind bool) (ports.WorkloadRequest, error) {
	wr := ports.WorkloadRequest{
		Name:          RuntimeName(release, name),
		Labels:        lbl,
		Image:         s.Image,
		Command:       s.Command,
		Args:          s.Args,
		Env:           s.Environment,
		RestartPolicy: s.RestartPolicy,
		StopTimeout:   s.StopGrace(),
	}
	for _, n := range s.Networks {
		wr.Networks = append(wr.Networks, RuntimeName(release, n))
	}
	for _, m := range s.Mounts {
		kind, target := m.Target()
		if kind == resource.KindVolume {
			wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountVolume, Source: RuntimeName(release, target), Target: m.MountPath, ReadOnly: m.ReadOnly})
			continue
		}
		dir, ok := paths[Key{kind, target}]
		if !ok {
			return wr, fmt.Errorf("mount %s: %s %q was not materialized", m.MountPath, kind, target)
		}
		wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountBind, Source: dir, Target: m.MountPath, ReadOnly: true})
	}
	if behind {
		rev, _ := strconv.Atoi(lbl[ports.LabelRevision])
		wr.Aliases = []string{BackendAlias(name, rev)}
	}
	if hc := s.HealthCheck; hc != nil && hc.Type == resource.HealthExec {
		// Durations were validated when the resource was decoded.
		interval, _ := time.ParseDuration(hc.Interval)
		timeout, _ := time.ParseDuration(hc.Timeout)
		start, _ := time.ParseDuration(hc.StartPeriod)
		wr.HealthCheck = &ports.HealthCheck{Command: hc.Command, Interval: interval, Timeout: timeout, Retries: hc.Retries, StartPeriod: start}
	}
	wr.NoNewPrivileges = true
	if sec := s.Security; sec != nil {
		wr.NoNewPrivileges = sec.NoNewPrivileges == nil || *sec.NoNewPrivileges
		wr.CapDrop, wr.CapAdd, wr.User = sec.CapDrop, sec.CapAdd, sec.User
		if sec.ReadOnlyRootFilesystem {
			wr.ReadOnlyRootfs = true
			wr.Tmpfs = map[string]string{"/tmp": "rw,nosuid,nodev,size=64m"}
		}
	}
	wr.PidsLimit = resource.DefaultPids
	wr.LogMaxSize, wr.LogMaxFiles = resource.DefaultLogMaxSize, resource.DefaultLogMaxFiles
	if l := s.Logging; l != nil && l.MaxSize != "" {
		wr.LogMaxSize, wr.LogMaxFiles = l.MaxSize, l.MaxFiles
	}
	if r := s.Resources; r != nil && r.Pids != nil {
		wr.PidsLimit = *r.Pids
	}
	if r := s.Resources; r != nil {
		if r.Memory != "" {
			wr.MemoryBytes, _ = resource.ParseMemory(string(r.Memory))
		}
		if r.CPU != "" {
			wr.MilliCPU, _ = resource.ParseCPU(string(r.CPU))
		}

	}
	return wr, nil
}

// pollInterval is how often waitReady checks workload status.
var pollInterval = 500 * time.Millisecond

// Delete removes every managed object of release: workloads first, then networks. Volumes are kept
// unless deleteVolumes is set or the volume's retention is delete. Materialized files under home are removed.
func Delete(ctx context.Context, rt ports.Runtime, home, release string, deleteVolumes bool) error {
	objs, err := rt.ListManaged(ctx, release)
	if err != nil {
		return err
	}
	var errs []error
	for _, kind := range []string{ports.ObjectWorkload, ports.ObjectNetwork, ports.ObjectVolume} {
		for _, o := range objs {
			if o.Kind != kind {
				continue
			}
			switch kind {
			case ports.ObjectWorkload:
				if err := rt.StopWorkload(ctx, o.Name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
					errs = append(errs, err)
				}
				errs = append(errs, deleteWorkload(ctx, rt, o.Name))
			case ports.ObjectNetwork:
				errs = append(errs, rt.DeleteNetwork(ctx, o.Name))
			case ports.ObjectVolume:
				if deletesVolume(o, deleteVolumes) {
					errs = append(errs, rt.DeleteVolume(ctx, o.Name))
				}
			}
		}
	}
	if home != "" {
		errs = append(errs, os.RemoveAll(filepath.Join(home, "releases", release)), os.RemoveAll(ProxyDir(home, release)))
		if proxySupported.Load() {
			_, err := gateway.Sync(ctx, rt, Proxies, home)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deletesVolume reports whether an uninstall removes volume o.
func deletesVolume(o ports.Object, deleteVolumes bool) bool {
	return deleteVolumes || o.Labels[ports.LabelRetention] == "delete"
}

// DeletePlan is what Delete would do to the runtime objects of release.
func DeletePlan(ctx context.Context, rt ports.Runtime, release string, deleteVolumes bool) (api.Plan, error) {
	objs, err := rt.ListManaged(ctx, release)
	if err != nil {
		return api.Plan{}, err
	}
	plan := api.Plan{Changes: []api.Change{}}
	kinds := map[string]string{ports.ObjectWorkload: resource.KindWorkload, ports.ObjectNetwork: resource.KindNetwork, ports.ObjectVolume: resource.KindVolume}
	for _, kind := range []string{ports.ObjectWorkload, ports.ObjectNetwork, ports.ObjectVolume} {
		for _, o := range objs {
			if o.Kind != kind {
				continue
			}
			c := api.Change{Kind: kinds[kind], Name: o.Name, Action: api.ActionDelete}
			if kind == ports.ObjectVolume {
				if deletesVolume(o, deleteVolumes) {
					c.Notice = "its data is deleted"
				} else {
					c.Action, c.Notice = api.ActionUnchanged, "kept with its data (--delete-volumes deletes it)"
				}
			}
			plan.Changes = append(plan.Changes, c)
		}
	}
	return plan, nil
}
