package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Execute applies a plan with the recreate strategy (D2):
//  1. pre-flight, materialize files, and ensure every needed image before touching anything running;
//  2. delete removed workloads (frees their ports);
//  3. create and update in dependency order. A recreate update is stop, delete, create, start; a
//     replace update starts the new container next to the old one and switches over (replaceStart);
//  4. wait for recreated workloads; ones that do not become ready are stopped, not removed (D14);
//  5. delete removed networks and volumes (volumes only with retention delete).
//
// Errors after the pre-flight phase are *ExecError.
func (e *Env) Execute(ctx context.Context, rt ports.Runtime, req ExecRequest) error {
	ctx = withProgress(ctx, req.OnProgress)
	disrupted := false
	err := e.execute(ctx, rt, req, &disrupted)
	if err == nil {
		return nil
	}
	if disrupted && e.host.Supported() {
		// Point the gateway at what runs now; the error that got us here is the one to report.
		_, _ = gateway.Sync(ctx, rt, e.host, e.home)
	}
	return &ExecError{Err: err, Disrupted: disrupted}
}

func (e *Env) execute(ctx context.Context, rt ports.Runtime, req ExecRequest, disrupted *bool) error {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.OnStep == nil {
		req.OnStep = func(Change, release.Step) error { return nil }
	}
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
	if err := e.preflightLocked(ctx, rt, info, req); err != nil {
		return err
	}
	paths, err := Materialize(e.home, req.Release, req.Revision, req.Resources, e.redact)
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
		if err := rt.EnsureImage(ctx, e.host.Proxies.Container().Image, "if-not-present"); err != nil {
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
			if err := report(c, ActionDelete, proxyops.RemoveWorkload(ctx, rt, c.Applied.RuntimeName)); err != nil {
				return err
			}
		}
	}

	// Proxies that go away or are recreated release the workloads' aliases first.
	for _, c := range req.Plan.Changes {
		if c.Kind == KindProxy && (c.Action == ActionDelete || c.Strategy == proxyRecreate) {
			*disrupted = true
			if err := report(c, ActionDelete, removeProxy(ctx, rt, e.home, req.Release, c, c.Action == ActionDelete)); err != nil {
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
		err := e.waitChanged(ctx, rt, req.Release, changed[waited:], req.Timeout, report)
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
				err = e.createProxy(ctx, rt, req, c, svcs)
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
		if err := e.writeProxy(ctx, rt, req, svcs, nil); err != nil {
			return err
		}
		progress(ctx, c.Kind, c.Name, PhaseWait, "waiting for %s", strings.Join(w.DependsOn, ", "))
		want := map[string]string{}
		for _, d := range w.DependsOn {
			if dep := svcs.svc[d]; dep != nil {
				rev, err := revisionOf(ctx, rt, proxyops.RuntimeName(req.Release, dep.Workload))
				if err != nil {
					return fmt.Errorf("%s %q depends on service %s, whose workload %s does not run: %w", c.Kind, c.Name, d, dep.Workload, err)
				}
				maps.Copy(want, proxyWant(svcs, dep.Workload, BackendAlias(dep.Workload, rev)))
			}
		}
		return envoyErr(ctx, rt, ProxyName(req.Release), e.host.Proxies.WaitReady(ctx, ProxyDir(e.home, req.Release), want, req.Timeout))
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
		name := proxyops.RuntimeName(req.Release, c.Name)
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
				touched, rerr = e.replaceWorkload(ctx, rt, req, c, owned, s, lbl, paths, svcs)
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
		if err := e.writeProxy(ctx, rt, req, svcs, nil); err != nil {
			return err
		}
		want := map[string]string{}
		for _, c := range req.Plan.Changes {
			changedService := c.Kind == resource.KindService && c.Action != ActionUnchanged && c.Action != ActionDelete
			if svcs.wl[c.Name] != nil && c.Kind == resource.KindWorkload && (waitAll || c.Action != ActionUnchanged) {
				if rev, err := revisionOf(ctx, rt, proxyops.RuntimeName(req.Release, c.Name)); err == nil {
					maps.Copy(want, proxyWant(svcs, c.Name, BackendAlias(c.Name, rev)))
				}
			}
			if s := svcs.svc[c.Name]; changedService && s != nil {
				if rev, err := revisionOf(ctx, rt, proxyops.RuntimeName(req.Release, s.Workload)); err == nil {
					maps.Copy(want, proxyWant(svcs, s.Workload, BackendAlias(s.Workload, rev)))
				}
			}
		}
		if len(want) > 0 {
			if err := envoyErr(ctx, rt, ProxyName(req.Release), e.host.Proxies.WaitReady(ctx, ProxyDir(e.home, req.Release), want, proxyops.ProxyReadyTimeout)); err != nil {
				return err
			}
		}
	}
	if e.host.Supported() {
		progress(ctx, "Gateway", gateway.ContainerName, PhaseSwitch, "updating the host gateway")
		targets, err := gateway.Expose(ctx, rt, e.host, e.home, req.Release, req.Resources, exposeRoutes(svcs, paths))
		if err == nil {
			err = envoyErr(ctx, rt, gateway.ContainerName, gateway.Wait(ctx, e.host, e.home, req.Release, targets))
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

// preflightLocked runs Preflight under the gateway lock: another release could otherwise
// bind a gateway port between this one listing the gateway's claims and test-binding the port.
func (e *Env) preflightLocked(ctx context.Context, rt ports.Runtime, info ports.RuntimeInfo, req ExecRequest) error {
	if e.host.Supported() {
		unlock, err := gateway.Lock(e.home)
		if err != nil {
			return err
		}
		defer unlock()
	}
	err := e.Preflight(ctx, rt, info, req.Release, req.Supported, req.Resources)
	return warnings(err, req.OnWarning)
}

// warnings passes the gateway.Warnings in err to warn and returns the rest.
func warnings(err error, warn func(string)) error {
	if err == nil {
		return nil
	}
	var rest []error
	for _, e := range unjoin(err) {
		if gateway.IsWarning(e) {
			if warn != nil {
				warn(e.Error())
			}
			continue
		}
		rest = append(rest, e)
	}
	return errors.Join(rest...)
}

func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, unjoin(e)...)
		}
		return out
	}
	return []error{err}
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

// waitChanged waits for every changed workload. On failure the ones that are not ready are stopped
// (kept for inspection) and reported as failed, so the next plan recreates them.
func (e *Env) waitChanged(ctx context.Context, rt ports.Runtime, rel string, changed []Change, timeout time.Duration, report func(Change, string, error) error) error {
	targets := make([]readyTarget, len(changed))
	for i, c := range changed {
		w, _ := workloadOf(c)
		targets[i] = readyTarget{proxyops.RuntimeName(rel, c.Name), w}
	}
	notReady, werr := e.waitReady(ctx, rt, targets, timeout)
	if werr == nil {
		return nil
	}
	errs := []error{werr}
	for _, c := range changed {
		name := proxyops.RuntimeName(rel, c.Name)
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
