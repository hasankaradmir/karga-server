package deployment

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// recreateWorkload recreates a workload: the old container goes first, so ports and names are free.
// Its anonymous volumes (image VOLUME paths no Volume covers) move to the new container, as docker
// compose does, so their data survives the update.
func recreateWorkload(ctx context.Context, rt ports.Runtime, c Change, owned map[string]bool, rel string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string) error {
	name := c.RuntimeName
	wr, err := workloadRequest(rel, c.Name, name, s, lbl, paths)
	if err != nil {
		return err
	}
	old := ""
	if c.Applied != nil {
		old = c.Applied.RuntimeName
	}
	if old == name {
		// A repair of the same spec, or (rarely: 8 hex) a new digest with the old one's name: the
		// old container must go first.
		progress(ctx, c.Kind, c.Name, PhaseStop, "stopping and removing %s", old)
		if wr.Mounts, err = takeOver(ctx, rt, old, wr.Mounts); err != nil {
			return err
		}
		old = ""
	}
	// The new container is created while the old one still runs: a spec Docker refuses leaves the
	// old one serving. Anonymous volumes move to it; only one of the two runs at a time.
	if old != "" {
		if wr.Mounts, err = anonymousMounts(ctx, rt, old, wr.Mounts); err != nil {
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
	if old != "" {
		progress(ctx, c.Kind, c.Name, PhaseStop, "stopping %s", old)
		if err := rt.StopWorkload(ctx, old, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
			return err
		}
	}
	err = rt.StartWorkload(ctx, name)
	if old != "" {
		err = errors.Join(err, rt.DeleteWorkload(ctx, old))
	}
	return err
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
	return withAnonymous(st, mounts), nil
}

// anonymousMounts is withAnonymous for container name; a missing container adds none.
func anonymousMounts(ctx context.Context, rt ports.Runtime, name string, mounts []ports.Mount) ([]ports.Mount, error) {
	st, err := rt.GetWorkload(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return mounts, nil
	}
	if err != nil {
		return mounts, err
	}
	return withAnonymous(st, mounts), nil
}

// withAnonymous returns mounts with the anonymous volumes of st added at their paths (unless a
// mount already uses the path).
func withAnonymous(st ports.WorkloadStatus, mounts []ports.Mount) []ports.Mount {
	for _, a := range st.AnonymousVolumes {
		if !slices.ContainsFunc(mounts, func(m ports.Mount) bool { return m.Target == a.Target }) {
			mounts = append(mounts, ports.Mount{Type: ports.MountVolume, Source: a.Source, Target: a.Target})
		}
	}
	return mounts
}

// replaceWorkload updates a workload without downtime (ADR-0011): its clients reach it only
// through its Services, that is through the release proxy, so switching the proxy switches all of
// them, from the release's networks and through the gateway alike:
//  1. remove a leftover container of this release with the new name (an earlier attempt);
//  2. create the new container, named after its digest, with its revision's backend alias (nobody
//     resolves it yet), start it and wait until healthy; on failure stop it (kept for inspection)
//     and return untouched;
//  3. point the proxy at the new alias and wait until it routes there; on failure restore it;
//  4. wait until the proxy drained the old container (at most the drain timeout), stop it and
//     remove it. The new container keeps its name: nothing is renamed.
//
// A workload no Service targets receives no traffic: it is started next to the old one and the old
// one is stopped once the new one is healthy.
//
// touched reports whether anything that was serving traffic has been changed.
func (e *Env) replaceWorkload(ctx context.Context, rt ports.Runtime, req ExecRequest, c Change, owned map[string]bool,
	s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, svcs services, proxy string) (touched bool, err error) {
	next := c.RuntimeName
	behind := svcs.wl[c.Name] != nil
	if owned[ports.ObjectWorkload+"/"+next] {
		if err := proxyops.RemoveWorkload(ctx, rt, next); err != nil {
			return false, err
		}
	}
	wr, err := workloadRequest(req.Release, c.Name, next, s, lbl, paths)
	if err != nil {
		return false, err
	}
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
	if _, err := e.waitReady(ctx, rt, []readyTarget{{next, s}}, req.Timeout); err != nil {
		return false, stopNext(err)
	}

	_, drain := s.Strategy()
	if behind {
		progress(ctx, c.Kind, c.Name, PhaseSwitch, "switching traffic to %s", next)
		alias := BackendAlias(c.Name, req.Revision)
		if err := e.writeProxy(ctx, rt, req, svcs, map[string]string{c.Name: alias}); err != nil {
			return false, stopNext(err)
		}
		if err := proxyops.EnvoyErr(ctx, rt, proxy, e.host.Proxies.WaitReady(ctx, ProxyAdminDir(e.home, req.Release, proxy), proxyWant(svcs, c.Name, alias), proxyops.ProxyReadyTimeout)); err != nil {
			// The proxy may have routed to the new backend for a moment: count it as touched. The new
			// container stops first, so the proxy goes back to the old one, which still runs.
			err = stopNext(err)
			return true, errors.Join(err, e.writeProxy(ctx, rt, req, svcs, nil))
		}
		progress(ctx, c.Kind, c.Name, PhaseDrain, "waiting up to %s for connections to %s to finish", drain, c.Applied.RuntimeName)
		if _, err := e.host.Proxies.WaitDrained(ctx, ProxyAdminDir(e.home, req.Release, proxy), proxyClusters(svcs, c.Name), drain); err != nil {
			return true, fmt.Errorf("traffic already goes to %s, but whether connections to %s ended could not be checked, so %s keeps running: %w",
				next, c.Applied.RuntimeName, c.Applied.RuntimeName, err)
		}
	}
	old := c.Applied.RuntimeName
	progress(ctx, c.Kind, c.Name, PhaseStop, "stopping %s", old)
	if err := rt.StopWorkload(ctx, old, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return true, err
	}
	return true, proxyops.DeleteWorkload(ctx, rt, old)
}

// RepairReplace repairs what an interrupted replace leaves behind, before the next plan is made:
// another container of a workload next to the one the state records. The one the proxy routes to
// is kept (the recorded one unless it had switched) and the other removed; a kept new one is
// returned with its own name and digest, so the plan sees what runs.
func (e *Env) RepairReplace(ctx context.Context, rt ports.Runtime, rel string, applied []release.AppliedResource) ([]release.AppliedResource, error) {
	objs, err := rt.ListManaged(ctx, rel)
	if err != nil {
		return nil, err
	}
	workloads, err := keepRouted(applied, objs, replacement{
		kind: resource.KindWorkload,
		mine: func(a release.AppliedResource, o ports.Object) bool {
			return o.Labels[ports.LabelKind] == resource.KindWorkload && o.Labels[ports.LabelName] == a.Name
		},
		routed: func(a release.AppliedResource, o ports.Object) bool { return e.routedTo(ctx, rt, rel, a.Name, o.Name) },
		remove: func(name string) error { return proxyops.RemoveWorkload(ctx, rt, name) },
	})
	if err != nil {
		return nil, err
	}
	// An interrupted proxy replace leaves two proxies: the one the gateway routes to is kept.
	proxies, err := keepRouted(applied, objs, replacement{
		kind:   KindProxy,
		mine:   func(_ release.AppliedResource, o ports.Object) bool { return o.Labels[ports.LabelKind] == KindProxy },
		routed: func(_ release.AppliedResource, o ports.Object) bool { return e.gatewayRoutesTo(o) },
		remove: func(name string) error { return removeProxyContainer(ctx, rt, e.home, rel, name) },
	})
	if err != nil {
		return nil, err
	}
	return append(workloads, proxies...), nil
}

// replacement says how RepairReplace settles the containers of one kind: which are an applied
// resource's, which one traffic goes to, and how one is removed.
type replacement struct {
	kind   string
	mine   func(a release.AppliedResource, o ports.Object) bool
	routed func(a release.AppliedResource, o ports.Object) bool
	remove func(name string) error
}

// keepRouted settles, for each applied resource of r.kind, the containers an interrupted replace
// left: while the recorded one is what traffic goes to, the others go; once traffic went to
// another, the recorded one goes and that one is recorded. It returns the resources to record anew.
func keepRouted(applied []release.AppliedResource, objs []ports.Object, r replacement) ([]release.AppliedResource, error) {
	var repaired []release.AppliedResource
	for _, a := range applied {
		if a.Kind != r.kind {
			continue
		}
		recorded := false
		var others []ports.Object
		for _, o := range objs {
			switch {
			case o.Kind != ports.ObjectWorkload || !r.mine(a, o):
			case o.Name == a.RuntimeName:
				recorded = true
			default:
				others = append(others, o)
			}
		}
		for _, o := range others {
			if recorded && !r.routed(a, o) {
				if err := r.remove(o.Name); err != nil { // the old one still serves
					return nil, err
				}
				continue
			}
			if recorded {
				if err := r.remove(a.RuntimeName); err != nil { // traffic had switched
					return nil, err
				}
			}
			a.RuntimeName, a.Digest, recorded = o.Name, o.Labels[ports.LabelDigest], true
			repaired = append(repaired, a)
		}
	}
	return repaired, nil
}
