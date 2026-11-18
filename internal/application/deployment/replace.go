package deployment

import (
	"context"
	"errors"
	"slices"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// nextSuffix names the container a replace starts next to the current one.
const nextSuffix = "-next"

// recreateWorkload recreates a workload: the old container goes first, so ports and names are free.
// Its anonymous volumes (image VOLUME paths no Volume covers) move to the new container, as docker
// compose does, so their data survives the update.
func recreateWorkload(ctx context.Context, rt ports.Runtime, c Change, owned map[string]bool, rel string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, behind bool) error {
	name := proxyops.RuntimeName(rel, c.Name)
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
func (e *Env) replaceWorkload(ctx context.Context, rt ports.Runtime, req ExecRequest, c Change, owned map[string]bool,
	s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, svcs services) (touched bool, err error) {
	name := proxyops.RuntimeName(req.Release, c.Name)
	next := name + nextSuffix
	behind := svcs.wl[c.Name] != nil
	if owned[ports.ObjectWorkload+"/"+next] {
		if err := proxyops.RemoveWorkload(ctx, rt, next); err != nil {
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
	if _, err := e.waitReady(ctx, rt, []readyTarget{{next, s}}, req.Timeout); err != nil {
		return false, stopNext(err)
	}

	_, drain := s.Strategy()
	if behind {
		progress(ctx, c.Kind, c.Name, PhaseSwitch, "switching traffic to %s", next)
		alias := BackendAlias(c.Name, req.Revision)
		if err := writeProxy(ctx, rt, e.Proxies, req, svcs, map[string]string{c.Name: alias}); err != nil {
			return false, stopNext(err)
		}
		if err := envoyErr(ctx, rt, ProxyName(req.Release), e.Proxies.WaitReady(ctx, ProxyDir(req.Home, req.Release), proxyWant(svcs, c.Name, alias), proxyops.ProxyReadyTimeout)); err != nil {
			// The proxy may have routed to the new backend for a moment: count it as touched.
			return true, stopNext(errors.Join(err, writeProxy(ctx, rt, e.Proxies, req, svcs, nil)))
		}
		progress(ctx, c.Kind, c.Name, PhaseDrain, "waiting up to %s for connections to %s to finish", drain, c.Applied.RuntimeName)
		if _, err := e.Proxies.WaitDrained(ctx, ProxyDir(req.Home, req.Release), proxyClusters(svcs, c.Name), drain); err != nil {
			return true, err
		}
	}
	old := c.Applied.RuntimeName
	progress(ctx, c.Kind, c.Name, PhaseStop, "stopping %s", old)
	if err := rt.StopWorkload(ctx, old, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return true, err
	}
	if err := proxyops.DeleteWorkload(ctx, rt, old); err != nil {
		return true, err
	}
	return true, rt.RenameWorkload(ctx, next, name)
}

// RepairReplace repairs what an interrupted replace leaves behind, before the next plan is made:
// with both <name> and <name>-next present the one the proxies route to is kept (the old one
// unless they had switched); with only <name>-next it takes the canonical name and is returned with
// an empty digest so the plan applies the workload again.
func (e *Env) RepairReplace(ctx context.Context, rt ports.Runtime, home, rel string, applied []release.AppliedResource) ([]release.AppliedResource, error) {
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
		if owned[ports.ObjectWorkload+"/"+a.RuntimeName] && e.routedTo(ctx, rt, home, rel, a.Name, next) {
			if err := proxyops.RemoveWorkload(ctx, rt, a.RuntimeName); err != nil {
				return nil, err
			}
			delete(owned, ports.ObjectWorkload+"/"+a.RuntimeName)
		}
		if owned[ports.ObjectWorkload+"/"+a.RuntimeName] {
			if err := proxyops.RemoveWorkload(ctx, rt, next); err != nil {
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
