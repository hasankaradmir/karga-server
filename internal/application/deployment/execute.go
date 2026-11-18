package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
		_, _ = gateway.New(rt, e.host, e.home).Sync(ctx)
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
	r := &run{e: e, rt: rt, req: req}
	defer func() { *disrupted = r.disrupted }()
	for _, phase := range []func(context.Context) error{
		r.prepare, r.removeWorkloads, r.removeProxies, r.apply, r.waitPending, r.proxyUp, r.routeProxy, r.expose, r.deleteRemoved,
	} {
		if err := phase(ctx); err != nil {
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
	warnings, err := e.Preflight(ctx, rt, info, req.Release, req.Supported, req.Resources)
	if req.OnWarning != nil {
		for _, w := range warnings {
			req.OnWarning(w)
		}
	}
	return err
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
	_ = json.Unmarshal(spec, &v) // a spec karga stored; unreadable keeps the volume (retention "")
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
