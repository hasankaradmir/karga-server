package releases

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Reconciliation brings the runtime back to a release's current revision without changing the
// revision (ADR-0012): it never upgrades, rolls back or edits values; that is the user's.
//
//   - a workload, proxy, network or volume missing in the runtime, or a container created from another
//     spec (changed outside karga), is created again from the current revision;
//   - a stopped workload or proxy is started; one Docker reports unhealthy, or whose tcp/http check
//     failed `retries` times in a row, is restarted;
//   - the gateway is started again if something of the release is exposed.
//
// Releases that are paused, failed (the user decides how to go on) or busy with an operation are
// left alone. Each repair backs off (10s doubling to 5m); after 5 failures in 10 minutes karga gives
// up on it until the user acts on the release or runs karga reconcile.

// ReconcileOperation is the operation type of reconciliation.
const ReconcileOperation = "reconcile"

// Repair backoff.
const (
	repairFirstDelay = 10 * time.Second
	repairMaxDelay   = 5 * time.Minute
	repairWindow     = 10 * time.Minute
	repairMaxFails   = 5
)

type repair struct {
	failures []time.Time
	delay    time.Duration
	next     time.Time
	lastErr  string
	probes   int // consecutive failed tcp/http checks
}

// repairs is the reconciler's memory per release/kind/name; it lives as long as the server.
type repairs struct {
	mu sync.Mutex
	m  map[string]*repair
}

func (r *repairs) get(key string) *repair {
	if r.m == nil {
		r.m = map[string]*repair{}
	}
	if r.m[key] == nil {
		r.m[key] = &repair{}
	}
	return r.m[key]
}

// reset forgets the failures of a release (the user acted on it).
func (r *repairs) reset(rel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.m {
		if len(k) > len(rel) && k[:len(rel)+1] == rel+"/" {
			delete(r.m, k)
		}
	}
}

func (r *repairs) recent(p *repair, now time.Time) int {
	n := 0
	for _, t := range p.failures {
		if now.Sub(t) < repairWindow {
			n++
		}
	}
	return n
}

// allowed says whether key may be repaired now, and whether karga gave up on it.
func (r *repairs) allowed(key string, now time.Time) (ok, gaveUp bool, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.get(key)
	if r.recent(p, now) >= repairMaxFails {
		return false, true, p.lastErr
	}
	return !now.Before(p.next), false, ""
}

func (r *repairs) done(key string, now time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.get(key)
	if err == nil {
		p.delay, p.next, p.probes = 0, time.Time{}, 0
		return
	}
	p.failures = append(p.failures, now)
	p.delay = min(max(2*p.delay, repairFirstDelay), repairMaxDelay)
	p.next, p.lastErr = now.Add(p.delay), err.Error()
}

// probeFailed counts a failed tcp/http check and says whether retries is reached.
func (r *repairs) probeFailed(key string, retries int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.get(key)
	p.probes++
	return p.probes >= retries
}

func (r *repairs) probeOK(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(key).probes = 0
}

// next is the earliest time a repair of rel that is waiting out its backoff may run again (zero if none).
func (r *repairs) next(rel string, now time.Time) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var at time.Time
	for k, p := range r.m {
		if !strings.HasPrefix(k, rel+"/") || !p.next.After(now) || r.recent(p, now) >= repairMaxFails {
			continue
		}
		if at.IsZero() || p.next.Before(at) {
			at = p.next
		}
	}
	return at
}

// NextRepair is when a failed repair of release name may be tried again (zero if none waits): a
// loop that reacts to events only must come back then, as no event may follow.
func (s *Lifecycle) NextRepair(name string) time.Time { return s.repairs.next(name, s.Now()) }

// gaveUp lists what karga stopped repairing in a release.
func (r *repairs) gaveUp(rel string, now time.Time) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, p := range r.m {
		if len(k) > len(rel) && k[:len(rel)+1] == rel+"/" && r.recent(p, now) >= repairMaxFails {
			out[k[len(rel)+1:]] = p.lastErr
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// planned is one repair the observation found.
type planned struct {
	key, kind, name, action, container string
	// failed is the applied state of a workload whose last apply failed (its digest is empty).
	failed *release.AppliedResource
}

// Repair brings release name's runtime back to its current revision. force forgets earlier
// failures first (karga reconcile).
func (s *Lifecycle) Repair(ctx context.Context, name string, force bool) (*ReconcileResult, error) {
	// What is observed below is acted on: nothing else may change the release meanwhile. A busy
	// release is skipped; the command working on it leaves it as it should be.
	unlock, err := s.lock(ctx, name, true)
	if errors.Is(err, ErrLocked) {
		return &ReconcileResult{Release: name, Skipped: "an operation is running"}, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return nil, err
	}
	res := &ReconcileResult{Release: name}
	if force {
		s.repairs.reset(name)
	}
	switch {
	case rel.Paused:
		res.Skipped = "paused"
	case rel.Status != release.StatusDeployed || rel.CurrentRevision == 0:
		res.Skipped = fmt.Sprintf("the release is %s; karga reconciles deployed releases only", rel.Status)
	}
	if _, err := s.Store.RunningOperation(ctx, name); err == nil {
		res.Skipped = "an operation is running"
	} else if !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}
	if res.Skipped != "" {
		return res, nil
	}
	rt, err := s.runtime(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.loadKey(); err != nil {
		return nil, err
	}
	applied, err := s.Store.ListResources(ctx, name)
	if err != nil {
		return nil, err
	}
	present, err := existing(ctx, rt, name, applied)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	var todo []planned
	add := func(p planned) {
		ok, gaveUp, _ := s.repairs.allowed(p.key, now)
		if ok && !gaveUp {
			todo = append(todo, p)
		}
	}
	missing := false
	for _, a := range applied {
		if !slices.ContainsFunc(present, func(p release.AppliedResource) bool { return p.Kind == a.Kind && p.Name == a.Name }) {
			missing = true
		}
	}
	if missing {
		add(planned{key: name + "/plan", action: "create"})
	}
	exposed := false
	for _, a := range present {
		if a.Kind == resource.KindService {
			if r, err := resource.DecodeStored(a.Spec); err == nil && len(r.Spec.(*resource.ServiceSpec).Expose) > 0 {
				exposed = true
			}
		}
		if a.Kind != resource.KindWorkload && a.Kind != deployment.KindProxy {
			continue
		}
		key := name + "/" + a.Kind + "/" + a.Name
		st, err := rt.GetWorkload(ctx, a.RuntimeName)
		if err != nil {
			continue
		}
		var failed *release.AppliedResource
		if a.Kind == resource.KindWorkload && a.Digest == "" {
			failed = &a
		}
		switch {
		case st.State != ports.StateRunning:
			add(planned{key, a.Kind, a.Name, "start", a.RuntimeName, failed})
		case st.Health == ports.HealthUnhealthy:
			add(planned{key, a.Kind, a.Name, "restart", a.RuntimeName, failed})
		case a.Kind == resource.KindWorkload:
			r, err := resource.DecodeStored(a.Spec)
			w, ok := r.Spec.(*resource.WorkloadSpec)
			if err != nil || !ok || w.HealthCheck == nil || w.HealthCheck.Type == resource.HealthExec {
				continue
			}
			if s.Deploy.ProbeHealth(ctx, st, w) == ports.HealthUnhealthy {
				retries := w.HealthCheck.Retries
				if retries == 0 {
					retries = 3
				}
				if s.repairs.probeFailed(key, retries) {
					add(planned{key, a.Kind, a.Name, "restart", a.RuntimeName, failed})
				}
			} else {
				s.repairs.probeOK(key)
			}
		}
	}
	if exposed {
		if st, err := rt.GetWorkload(ctx, gateway.ContainerName); err != nil || st.State != ports.StateRunning {
			add(planned{key: name + "/gateway", kind: "Gateway", name: gateway.ContainerName, action: "sync"})
		}
	}
	res.GaveUp = s.repairs.gaveUp(name, now)
	if len(todo) == 0 {
		return res, nil
	}

	op, err := s.begin(ctx, name, ReconcileOperation, rel.CurrentRevision, false, release.Options{})
	if err != nil {
		if errors.Is(err, ErrLocked) || errors.Is(err, ErrInterrupted) {
			res.Skipped = "an operation is running"
			return res, nil
		}
		return nil, err
	}
	var errs []error
	step := func(p planned, err error) {
		st := release.Step{Kind: p.kind, Name: p.name, Action: p.action, Status: deployment.StepDone}
		if err != nil {
			st.Status, st.Error = deployment.StepFailed, err.Error()
			errs = append(errs, fmt.Errorf("%s %s: %s: %w", p.kind, p.name, p.action, err))
		}
		res.Actions = append(res.Actions, st)
		op.Steps = append(op.Steps, st)
		s.repairs.done(p.key, s.Now(), err)
		if err == nil && p.failed != nil {
			s.healed(ctx, rt, name, *p.failed)
		}
	}
	for _, p := range todo {
		switch p.action {
		case "start":
			step(p, rt.StartWorkload(ctx, p.container))
		case "restart":
			err := rt.StopWorkload(ctx, p.container, 0)
			if err == nil {
				err = rt.StartWorkload(ctx, p.container)
			}
			step(p, err)
		case "sync":
			_, err := gateway.Sync(ctx, rt, s.Deploy.Host, s.Home)
			step(p, err)
		case "create":
			steps, err := s.recreate(ctx, rt, &op, rel, present)
			res.Actions = append(res.Actions, steps...)
			if err != nil {
				errs = append(errs, err)
			}
			s.repairs.done(p.key, s.Now(), err)
		}
	}
	s.finish(ctx, op, errors.Join(errs...))
	res.GaveUp = s.repairs.gaveUp(name, s.Now())
	return res, nil
}

// recreate creates what is missing from the current revision, through the same plan and executor as
// an upgrade, without a new revision. Only creates are applied: anything else that differs (a newer
// karga's defaults, for example) waits for the user's next upgrade.
func (s *Lifecycle) recreate(ctx context.Context, rt ports.Runtime, op *release.Operation, rel release.Release, present []release.AppliedResource) ([]release.Step, error) {
	rev, err := s.Store.GetRevision(ctx, rel.Name, rel.CurrentRevision)
	if err != nil {
		return nil, err
	}
	rs, err := decodeManifest(rev.Manifest)
	if err != nil {
		return nil, err
	}
	if err := s.restoreSecrets(rel.Name, rev.Number, rs); err != nil {
		return nil, err
	}
	anon, err := anonymousVolumes(ctx, rt, rel.Name, rs)
	if err != nil {
		return nil, err
	}
	plan, err := s.Deploy.Diff(rel.Name, rs, present, "", anon)
	if err != nil {
		return nil, err
	}
	for i := range plan.Changes {
		if c := &plan.Changes[i]; c.Action != deployment.ActionCreate {
			c.Action = deployment.ActionUnchanged
		}
	}
	in := deployInput{Name: rel.Name, Type: ReconcileOperation, Revision: rel.CurrentRevision, Resources: rs,
		Package: pkgOf(rev), Existing: &rel}
	in.Timeout = deployment.DefaultTimeout
	start := len(op.Steps)
	err = s.Deploy.Execute(ctx, rt, deployment.ExecRequest{
		Home: s.Home, Release: rel.Name, Revision: rel.CurrentRevision, Supported: rev.RuntimeSupported,
		Resources: rs, Plan: plan, Redact: s.Redact, Timeout: in.Timeout, AllowedCaps: s.AllowedCaps,
		OnStep: func(c deployment.Change, st release.Step) error { return s.record(ctx, op, in, c, st) },
	})
	return slices.Clone(op.Steps[start:]), err
}

// healed marks a workload applied again once its container, created from the current revision by
// an earlier (interrupted) repair, runs: its digest label says which spec it runs.
func (s *Lifecycle) healed(ctx context.Context, rt ports.Runtime, rel string, a release.AppliedResource) {
	st, err := rt.GetWorkload(ctx, a.RuntimeName)
	if err != nil || st.State != ports.StateRunning || st.Labels[ports.LabelDigest] == "" {
		return
	}
	if rev, err := strconv.Atoi(st.Labels[ports.LabelRevision]); err != nil || rev != a.Revision {
		return // a container of another revision (a failed upgrade's) stays failed
	}
	a.Digest = st.Labels[ports.LabelDigest]
	if err := s.Store.SaveResource(context.WithoutCancel(ctx), rel, a); err != nil {
		s.Log.Warn("reconcile: record repaired workload", "release", rel, "workload", a.Name, "error", err)
	}
}

func pkgOf(rev release.Revision) packages.Metadata {
	return packages.Metadata{Name: rev.PackageName, Version: rev.PackageVersion, Runtime: packages.Runtime{Supported: rev.RuntimeSupported}}
}

// Pause stops (or with paused false, resumes) reconciliation of a release.
func (s *Lifecycle) Pause(ctx context.Context, name string, paused bool) error {
	unlock, err := s.lock(ctx, name, false)
	if err != nil {
		return err
	}
	defer unlock()
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return err
	}
	rel.Paused = paused
	if !paused {
		s.repairs.reset(name)
	}
	return s.Store.SaveRelease(ctx, rel)
}

// Events lists the operations of a release (all for ""), newest first.
func (s *Reads) Events(ctx context.Context, name string, afterID int64, limit int) ([]release.Operation, error) {
	return s.Store.ListOperations(ctx, name, afterID, limit)
}
