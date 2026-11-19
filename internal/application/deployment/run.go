package deployment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// run is one Execute: its request, what the phases learned so far, and whether it disrupted what
// was running (stopped, removed or created a workload, a proxy, the gateway's routes).
type run struct {
	e   *Env
	rt  ports.Runtime
	req ExecRequest

	svcs  services
	paths map[Key]string // materialized Config and Secret files
	owned map[string]bool

	disrupted bool
	changed   []Change // recreated workloads; changed[:waited] are ready
	waited    int
	proxyDone bool
	waitAll   bool // the proxy changed: every workload behind it is waited for
}

// report records the outcome of a change's step and wraps its error with the change.
func (r *run) report(ctx context.Context, c Change, action string, err error) error {
	st := release.Step{Kind: c.Kind, Name: c.Name, Action: action, Status: StepDone}
	if err != nil {
		st.Status, st.Error = StepFailed, err.Error()
		progress(ctx, c.Kind, c.Name, PhaseFailed, "%s: %v", action, err)
	} else {
		progress(ctx, c.Kind, c.Name, PhaseDone, "%s", action)
	}
	if serr := r.req.OnStep(c, st); serr != nil {
		return errors.Join(err, serr)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
	}
	return nil
}

// reporter is report bound to ctx, for helpers that report several changes.
func (r *run) reporter(ctx context.Context) func(Change, string, error) error {
	return func(c Change, action string, err error) error { return r.report(ctx, c, action, err) }
}

// prepare checks the plan can run and gets everything it needs before anything running changes:
// pre-flight, the Config and Secret files, the images, the release's objects.
func (r *run) prepare(ctx context.Context) error {
	info, err := r.rt.Info(ctx)
	if err != nil {
		return err
	}
	if err := r.e.preflightLocked(ctx, r.rt, info, r.req); err != nil {
		return err
	}
	if r.paths, err = Materialize(r.e.home, r.req.Release, r.req.Revision, r.req.Resources, r.e.redact); err != nil {
		return err
	}
	r.svcs = servicesOf(r.req.Resources)
	for _, c := range r.req.Plan.Changes {
		if c.Action != ActionCreate && c.Action != ActionUpdate {
			continue
		}
		if w, ok := workloadOf(c); ok {
			progress(ctx, c.Kind, c.Name, PhasePull, "image %s (%s)", w.Image, w.PullPolicy)
			if err := r.rt.EnsureImage(ctx, w.Image, w.PullPolicy); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
		if c.Kind == KindProxy {
			if err := r.rt.EnsureImage(ctx, c.Proxy.Image, "if-not-present"); err != nil {
				return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
			}
		}
	}
	if len(r.svcs.published()) > 0 {
		if err := r.rt.EnsureImage(ctx, r.e.host.Proxies.Container().Image, "if-not-present"); err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
	}
	r.owned, err = managedObjects(ctx, r.rt, r.req.Release)
	return err
}

// removeWorkloads removes the workloads the plan deletes.
func (r *run) removeWorkloads(ctx context.Context) error {
	for _, c := range r.req.Plan.Changes {
		if c.Action == ActionDelete && c.Kind == resource.KindWorkload {
			r.disrupted = true
			if err := r.report(ctx, c, ActionDelete, proxyops.RemoveWorkload(ctx, r.rt, c.Applied.RuntimeName)); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeProxies removes proxies that go away or are recreated: they release the workloads'
// aliases first.
func (r *run) removeProxies(ctx context.Context) error {
	for _, c := range r.req.Plan.Changes {
		if c.Kind == KindProxy && (c.Action == ActionDelete || c.Strategy == proxyRecreate) {
			r.disrupted = true
			if err := r.report(ctx, c, ActionDelete, removeProxy(ctx, r.rt, r.e.home, r.req.Release, c, c.Action == ActionDelete)); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitPending waits for the recreated workloads not waited for yet.
func (r *run) waitPending(ctx context.Context) error {
	if r.waited == len(r.changed) {
		return nil
	}
	err := r.e.waitChanged(ctx, r.rt, r.req.Release, r.changed[r.waited:], r.req.Timeout, r.reporter(ctx))
	r.waited = len(r.changed)
	return err
}

// proxyUp starts or changes the release proxy, once: after the workloads before it are ready,
// pointing at what runs now (at the end, or before the first workload that depends on a Service).
func (r *run) proxyUp(ctx context.Context) error {
	if r.proxyDone {
		return nil
	}
	r.proxyDone = true
	for _, c := range r.req.Plan.Changes {
		if c.Kind != KindProxy || (c.Action != ActionCreate && c.Action != ActionUpdate) {
			continue
		}
		r.disrupted, r.waitAll = true, true
		if c.Strategy == proxyReplace {
			progress(ctx, c.Kind, c.Name, PhaseCreate, "starting %s next to %s", c.RuntimeName, c.Applied.RuntimeName)
		}
		if err := r.report(ctx, c, c.Action, r.e.createProxy(ctx, r.rt, r.req, c, r.svcs, r.owned)); err != nil {
			return err
		}
	}
	return nil
}

// proxy is the release proxy that serves now: until proxyUp, the one that ran before the plan.
func (r *run) proxy() string {
	for _, c := range r.req.Plan.Changes {
		if c.Kind == KindProxy && c.Action != ActionDelete {
			if !r.proxyDone && c.Applied != nil {
				return c.Applied.RuntimeName
			}
			return c.RuntimeName
		}
	}
	return ""
}

// retireProxy drains and removes the proxy a replaced one took over from, once the gateway routes
// to the new one: in the release's networks both answer to the Services' names until then.
func (r *run) retireProxy(ctx context.Context) error {
	for _, c := range r.req.Plan.Changes {
		if c.Kind != KindProxy || c.Strategy != proxyReplace {
			continue
		}
		old := c.Applied.RuntimeName
		progress(ctx, c.Kind, c.Name, PhaseDrain, "waiting up to %s for connections to %s to finish", proxyops.ProxyDrainTimeout, old)
		if done, err := r.e.host.Proxies.Drain(ctx, ProxyAdminDir(r.e.home, r.req.Release, old), true, proxyops.ProxyDrainTimeout); err != nil || !done {
			progress(ctx, c.Kind, c.Name, PhaseDrain, "%s still had connections (%v); they are closed", old, err)
		}
		progress(ctx, c.Kind, c.Name, PhaseStop, "stopping %s", old)
		if err := removeProxy(ctx, r.rt, r.e.home, r.req.Release, c, false); err != nil {
			return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
		}
	}
	return nil
}

// dependencies waits until the Services workload c depends on are ready behind the proxy.
func (r *run) dependencies(ctx context.Context, c Change, w *resource.WorkloadSpec) error {
	if len(w.DependsOn) == 0 {
		return nil
	}
	if err := r.waitPending(ctx); err != nil {
		return err
	}
	if err := r.proxyUp(ctx); err != nil {
		return err
	}
	if err := r.e.writeProxy(ctx, r.rt, r.req, r.svcs, nil); err != nil {
		return err
	}
	progress(ctx, c.Kind, c.Name, PhaseWait, "waiting for %s", strings.Join(w.DependsOn, ", "))
	want := map[string]string{}
	for _, d := range w.DependsOn {
		if dep := r.svcs.svc[d]; dep != nil {
			rev, err := revisionOf(ctx, r.rt, r.req.Plan.containers(dep.Workload))
			if err != nil {
				return fmt.Errorf("%s %q depends on service %s, whose workload %s does not run: %w", c.Kind, c.Name, d, dep.Workload, err)
			}
			maps.Copy(want, proxyWant(r.svcs, dep.Workload, BackendAlias(dep.Workload, rev)))
		}
	}
	return envoyErr(ctx, r.rt, r.proxy(), r.e.host.Proxies.WaitReady(ctx, ProxyAdminDir(r.e.home, r.req.Release, r.proxy()), want, r.req.Timeout))
}

// apply creates and updates what the plan creates and updates, in order.
func (r *run) apply(ctx context.Context) error {
	for _, c := range r.req.Plan.Changes {
		if c.Kind == KindProxy || (c.Action != ActionCreate && c.Action != ActionUpdate) {
			continue
		}
		if err := r.applyOne(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) applyOne(ctx context.Context, c Change) error {
	if w, ok := workloadOf(c); ok {
		if err := r.dependencies(ctx, c, w); err != nil {
			return fmt.Errorf("%s %q: %w", c.Kind, c.Name, err)
		}
	}
	name := proxyops.RuntimeName(r.req.Release, c.Name)
	lbl := labels(r.req.Release, r.req.Revision, c.Kind, c.Name)
	var err error
	switch s := c.Resource.Spec.(type) {
	case *resource.NetworkSpec:
		err = ownedOK(r.owned, ports.ObjectNetwork, name,
			r.rt.CreateNetwork(ctx, ports.NetworkRequest{Name: name, Labels: lbl, Driver: s.Driver, Internal: s.Internal}))
	case *resource.VolumeSpec:
		lbl[ports.LabelRetention] = s.Retention
		err = ownedOK(r.owned, ports.ObjectVolume, name,
			r.rt.CreateVolume(ctx, ports.VolumeRequest{Name: name, Labels: lbl, Driver: s.Driver}))
	case *resource.WorkloadSpec:
		lbl[ports.LabelDigest] = c.Digest
		if c.Action == ActionUpdate && c.Strategy == resource.StrategyReplace {
			touched, rerr := r.e.replaceWorkload(ctx, r.rt, r.req, c, r.owned, s, lbl, r.paths, r.svcs, r.proxy())
			r.disrupted = r.disrupted || touched
			if rerr != nil && !touched {
				_ = r.req.OnStep(c, release.Step{Kind: c.Kind, Name: c.Name, Action: c.Action, Status: StepKept, Error: rerr.Error()})
				err := fmt.Errorf("%s %q: %w (the previous container is still running)", c.Kind, c.Name, rerr)
				// Workloads recreated earlier in this plan are waited for like on success: ones that do not
				// become ready are stopped and reported failed, so the next plan recreates them.
				return errors.Join(err, r.waitPending(ctx))
			}
			err = rerr
			break
		}
		r.disrupted = true
		err = recreateWorkload(ctx, r.rt, c, r.owned, r.req.Release, s, lbl, r.paths)
		r.changed = append(r.changed, c)
	}
	// Config and Secret have no runtime object: they were materialized in prepare.
	return r.report(ctx, c, c.Action, err)
}

// routeProxy points the release proxy at what runs now and waits until it routes there.
func (r *run) routeProxy(ctx context.Context) error {
	if len(r.svcs.svc) == 0 {
		return nil
	}
	if err := r.e.writeProxy(ctx, r.rt, r.req, r.svcs, nil); err != nil {
		return err
	}
	want := map[string]string{}
	for _, c := range r.req.Plan.Changes {
		changedService := c.Kind == resource.KindService && c.Action != ActionUnchanged && c.Action != ActionDelete
		if r.svcs.wl[c.Name] != nil && c.Kind == resource.KindWorkload && (r.waitAll || c.Action != ActionUnchanged) {
			if rev, err := revisionOf(ctx, r.rt, r.req.Plan.containers(c.Name)); err == nil {
				maps.Copy(want, proxyWant(r.svcs, c.Name, BackendAlias(c.Name, rev)))
			}
		}
		if s := r.svcs.svc[c.Name]; changedService && s != nil {
			if rev, err := revisionOf(ctx, r.rt, r.req.Plan.containers(s.Workload)); err == nil {
				maps.Copy(want, proxyWant(r.svcs, s.Workload, BackendAlias(s.Workload, rev)))
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	return envoyErr(ctx, r.rt, r.proxy(), r.e.host.Proxies.WaitReady(ctx, ProxyAdminDir(r.e.home, r.req.Release, r.proxy()), want, proxyops.ProxyReadyTimeout))
}

// expose updates what the host gateway routes to the release and waits until it routes so.
func (r *run) expose(ctx context.Context) error {
	if !r.e.host.Supported() {
		return nil
	}
	progress(ctx, "Gateway", gateway.ContainerName, PhaseSwitch, "updating the host gateway")
	targets, err := gateway.New(r.rt, r.e.host, r.e.home).Expose(ctx, r.req.Release, r.proxy(), r.req.Resources, exposeRoutes(r.svcs, r.paths))
	if err == nil {
		gw := gateway.New(r.rt, r.e.host, r.e.home)
		err = gw.Wait(ctx, r.req.Release, targets)
		if name, nerr := gw.Current(ctx); nerr == nil {
			err = envoyErr(ctx, r.rt, name, err)
		}
	}
	if err != nil {
		r.disrupted = true
		return fmt.Errorf("gateway: %w", err)
	}
	return nil
}

// deleteRemoved deletes the networks and volumes (retention: delete) the plan removes, last first.
func (r *run) deleteRemoved(ctx context.Context) error {
	for _, c := range reverse(r.req.Plan.Changes) {
		if c.Action != ActionDelete || c.Kind == resource.KindWorkload || c.Kind == KindProxy {
			continue
		}
		var err error
		switch c.Kind {
		case resource.KindNetwork:
			r.disrupted = true
			err = r.rt.DeleteNetwork(ctx, c.Applied.RuntimeName)
		case resource.KindVolume:
			if volumeRetention(c.Applied.Spec) == "delete" {
				r.disrupted = true
				err = r.rt.DeleteVolume(ctx, c.Applied.RuntimeName)
			}
		}
		if err := r.report(ctx, c, ActionDelete, err); err != nil {
			return err
		}
	}
	return nil
}
