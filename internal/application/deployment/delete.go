package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Delete removes every managed object of release: workloads first, then networks. Volumes are kept
// unless deleteVolumes is set or the volume's retention is delete. Materialized files under home are removed.
func (e *Env) Delete(ctx context.Context, rt ports.Runtime, release string, deleteVolumes bool) error {
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
				errs = append(errs, proxyops.DeleteWorkload(ctx, rt, o.Name))
			case ports.ObjectNetwork:
				errs = append(errs, rt.DeleteNetwork(ctx, o.Name))
			case ports.ObjectVolume:
				if deletesVolume(o, deleteVolumes) {
					errs = append(errs, rt.DeleteVolume(ctx, o.Name))
				}
			}
		}
	}
	if err := errors.Join(errs...); err != nil || e.home == "" {
		return err // containers that are left keep the files they mount: a new uninstall finishes
	}
	return errors.Join(os.RemoveAll(filepath.Join(e.home, "releases", release)), os.RemoveAll(ProxyDir(e.home, release)),
		gateway.ReleaseEdge(ctx, e.home, release))
}

// SyncGateway takes the routes of releases that are gone out of the gateway.
func (e *Env) SyncGateway(ctx context.Context, rt ports.Runtime) error {
	if e.home == "" || !e.host.Supported() {
		return nil
	}
	_, err := gateway.New(rt, e.host, e.home).Sync(ctx)
	return err
}

// deletesVolume reports whether an uninstall removes volume o.
func deletesVolume(o ports.Object, deleteVolumes bool) bool {
	return deleteVolumes || o.Labels[ports.LabelRetention] == "delete"
}

// DeletePlan is what Delete would do to the runtime objects of release.
func (e *Env) DeletePlan(ctx context.Context, rt ports.Runtime, release string, deleteVolumes bool) ([]Summary, error) {
	objs, err := rt.ListManaged(ctx, release)
	if err != nil {
		return nil, err
	}
	plan := []Summary{}
	kinds := map[string]string{ports.ObjectWorkload: resource.KindWorkload, ports.ObjectNetwork: resource.KindNetwork, ports.ObjectVolume: resource.KindVolume}
	for _, kind := range []string{ports.ObjectWorkload, ports.ObjectNetwork, ports.ObjectVolume} {
		for _, o := range objs {
			if o.Kind != kind {
				continue
			}
			c := Summary{Kind: kinds[kind], Name: o.Name, Action: ActionDelete}
			if kind == ports.ObjectVolume {
				if deletesVolume(o, deleteVolumes) {
					c.Notice = "its data is deleted"
				} else {
					c.Action, c.Notice = ActionUnchanged, "kept with its data (--delete-volumes deletes it)"
				}
			}
			plan = append(plan, c)
		}
	}
	return plan, nil
}
