package server

import (
	"context"
	"io"
	"time"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Backend serves the API (karga-sdk/api) with the release service: it maps the wire's requests to
// the service's and the service's answers to the wire's views. Nothing else knows the API.
type Backend struct{ S *releases.Service }

var _ api.Backend = Backend{}

func common(o api.CommonOptions) releases.CommonOptions {
	return releases.CommonOptions{DryRun: o.DryRun, Timeout: time.Duration(o.Timeout), HistoryMax: o.HistoryMax, Force: o.Force, DeleteVolumes: o.DeleteVolumes}
}

func values(v api.ValuesInput) releases.Values {
	return releases.Values{Files: v.Files, Set: v.Set, SetString: v.SetString, Strict: v.Strict}
}

func events(progress func(api.Event)) func(deployment.Event) {
	if progress == nil {
		return nil
	}
	return func(e deployment.Event) {
		progress(api.Event{Kind: e.Kind, Name: e.Name, Phase: e.Phase, Detail: e.Detail})
	}
}

// Install implements api.Backend.
func (b Backend) Install(ctx context.Context, req api.InstallRequest, progress func(api.Event)) (*api.Result, error) {
	res, err := b.S.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: common(req.CommonOptions), Name: req.Name, Package: req.Package,
		Values: values(req.Values)}, events(progress))
	return wireResult(res), err
}

// Upgrade implements api.Backend.
func (b Backend) Upgrade(ctx context.Context, req api.UpgradeRequest, progress func(api.Event)) (*api.Result, error) {
	res, err := b.S.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common(req.CommonOptions), Name: req.Name, Package: req.Package,
		Values: values(req.Values), ReuseValues: req.ReuseValues, Install: req.Install, Strategy: req.Strategy}, events(progress))
	return wireResult(res), err
}

// Rollback implements api.Backend.
func (b Backend) Rollback(ctx context.Context, req api.RollbackRequest, progress func(api.Event)) (*api.Result, error) {
	res, err := b.S.Lifecycle.Rollback(ctx, releases.RollbackRequest{CommonOptions: common(req.CommonOptions), Name: req.Name,
		Revision: req.Revision, Strategy: req.Strategy}, events(progress))
	return wireResult(res), err
}

// Uninstall implements api.Backend.
func (b Backend) Uninstall(ctx context.Context, req api.UninstallRequest) (*api.Result, error) {
	res, err := b.S.Lifecycle.Uninstall(ctx, releases.UninstallRequest{Name: req.Name, Force: req.Force, DeleteVolumes: req.DeleteVolumes,
		KeepHistory: req.KeepHistory, DryRun: req.DryRun})
	return wireResult(res), err
}

// List implements api.Backend.
func (b Backend) List(ctx context.Context) ([]api.Release, error) {
	rels, err := b.S.Reads.List(ctx)
	return wireAll(rels, wireRelease), err
}

// History implements api.Backend.
func (b Backend) History(ctx context.Context, name string) ([]api.Revision, error) {
	revs, err := b.S.Reads.History(ctx, name)
	return wireAll(revs, wireRevision), err
}

// GetValues implements api.Backend.
func (b Backend) GetValues(ctx context.Context, name string, revision int, all bool) (map[string]any, error) {
	return b.S.Reads.GetValues(ctx, name, revision, all)
}

// GetNotes implements api.Backend.
func (b Backend) GetNotes(ctx context.Context, name string, revision int) (string, error) {
	return b.S.Reads.GetNotes(ctx, name, revision)
}

// GetManifest implements api.Backend.
func (b Backend) GetManifest(ctx context.Context, name string, revision int) ([]resource.Resource, error) {
	return b.S.Reads.GetManifest(ctx, name, revision)
}

// Status implements api.Backend.
func (b Backend) Status(ctx context.Context, name string) (*api.StatusReport, error) {
	rep, err := b.S.Reads.Status(ctx, name)
	if rep == nil {
		return nil, err
	}
	out := &api.StatusReport{Release: wireRelease(rep.Release), Resources: []api.ResourceStatus{}, Orphans: rep.Orphans, Interrupted: rep.Interrupted}
	for _, r := range rep.Resources {
		out.Resources = append(out.Resources, api.ResourceStatus{Kind: r.Kind, Name: r.Name, RuntimeName: r.RuntimeName, Revision: r.Revision,
			Desired: r.Desired, Actual: r.Actual, Health: r.Health, Backend: r.Backend, Restarts: r.Restarts, OOMKilled: r.OOMKilled})
	}
	if rc := rep.Reconcile; rc != nil {
		out.Reconcile = &api.ReconcileStatus{Last: rc.Last, Status: rc.Status, Error: rc.Error, GaveUp: rc.GaveUp}
	}
	return out, err
}

// Diff implements api.Backend.
func (b Backend) Diff(ctx context.Context, name string, from, to int) (*api.RevisionDiff, error) {
	d, err := b.S.Reads.Diff(ctx, name, from, to)
	if d == nil {
		return nil, err
	}
	out := &api.RevisionDiff{Release: d.Release, From: d.From, To: d.To, Package: wireFields(d.Package), Values: wireFields(d.Values),
		Resources: []api.ResourceDiff{}}
	for _, r := range d.Resources {
		out.Resources = append(out.Resources, api.ResourceDiff{Kind: r.Kind, Name: r.Name, Action: r.Action, Fields: wireFields(r.Fields)})
	}
	return out, err
}

// Resources implements api.Backend.
func (b Backend) Resources(ctx context.Context, name string) ([]api.AppliedResource, error) {
	rs, err := b.S.Reads.Resources(ctx, name)
	return wireAll(rs, func(a release.AppliedResource) api.AppliedResource {
		return api.AppliedResource{Kind: a.Kind, Name: a.Name, Revision: a.Revision, RuntimeName: a.RuntimeName}
	}), err
}

// Logs implements api.Backend.
func (b Backend) Logs(ctx context.Context, req api.LogsRequest, w io.Writer) error {
	return b.S.Reads.Logs(ctx, releases.LogsRequest{Release: req.Release, Workloads: req.Workloads, Proxy: req.Proxy, Gateway: req.Gateway,
		Tail: req.Tail, Follow: req.Follow}, w)
}

// Exec implements api.Backend.
func (b Backend) Exec(ctx context.Context, rel, workload string, o api.ExecOptions) (int, error) {
	return b.S.Reads.Exec(ctx, rel, workload, ports.ExecOptions{Cmd: o.Cmd, TTY: o.TTY, Stdin: o.Stdin, Stdout: o.Stdout, Stderr: o.Stderr,
		Width: o.Width, Height: o.Height, Resize: o.Resize})
}

// BackupVolume implements api.Backend.
func (b Backend) BackupVolume(ctx context.Context, req api.VolumeBackupRequest, w io.Writer) error {
	return b.S.Volumes.BackupVolume(ctx, releases.VolumeBackupRequest{Release: req.Release, Volume: req.Volume, Stop: req.Stop}, w)
}

// RestoreVolume implements api.Backend.
func (b Backend) RestoreVolume(ctx context.Context, req api.VolumeRestoreRequest, r io.Reader) error {
	return b.S.Volumes.RestoreVolume(ctx, releases.VolumeRestoreRequest{Release: req.Release, Volume: req.Volume, Force: req.Force, Name: req.Name}, r)
}

// Reconcile implements api.Backend.
func (b Backend) Reconcile(ctx context.Context, name string, force bool) (*api.ReconcileResult, error) {
	res, err := b.S.Lifecycle.Repair(ctx, name, force)
	if res == nil {
		return nil, err
	}
	return &api.ReconcileResult{Release: res.Release, Skipped: res.Skipped, Actions: wireAll(res.Actions, wireStep), GaveUp: res.GaveUp}, err
}

// Pause implements api.Backend.
func (b Backend) Pause(ctx context.Context, name string, paused bool) error {
	return b.S.Lifecycle.Pause(ctx, name, paused)
}

// Events implements api.Backend.
func (b Backend) Events(ctx context.Context, name string, afterID int64, limit int) ([]api.Operation, error) {
	ops, err := b.S.Reads.Events(ctx, name, afterID, limit)
	return wireAll(ops, func(o release.Operation) api.Operation {
		return api.Operation{ID: o.ID, Release: o.Release, Type: o.Type, Revision: o.Revision, Status: string(o.Status),
			Actor: o.Actor, Steps: wireAll(o.Steps, wireStep), Error: o.Error, StartedAt: o.StartedAt, FinishedAt: o.FinishedAt}
	}), err
}

// Gateway implements api.Backend.
func (b Backend) Gateway(ctx context.Context) ([]api.Exposed, error) {
	routes, err := b.S.Gateway.Routes(ctx)
	return wireAll(routes, func(e gateway.Exposed) api.Exposed {
		return api.Exposed{Listener: e.Listener, Route: e.Route, Release: e.Release, Service: e.Service, Upstream: e.Upstream}
	}), err
}

// GatewayHealth implements api.Backend.
func (b Backend) GatewayHealth(ctx context.Context) (api.State, error) {
	st, err := b.S.Gateway.Health(ctx)
	return api.State{Container: st.Container, Ready: st.Ready, Version: st.Version, Image: st.Image, Listen: st.Listen,
		AccessLog: st.AccessLog, Error: st.Error}, err
}

// GatewaySettings implements api.Backend.
func (b Backend) GatewaySettings(ctx context.Context) (api.Settings, map[string][]string, error) {
	g, users, err := b.S.Gateway.Settings(ctx)
	out := api.Settings{Listen: g.Listen, AccessLog: g.AccessLog, Listeners: []api.Listener{}}
	for _, l := range g.Listeners {
		out.Listeners = append(out.Listeners, api.Listener{Name: l.Name, Port: l.Port, Protocol: l.Protocol})
	}
	return out, users, err
}

// SetGatewayAccessLog implements api.Backend.
func (b Backend) SetGatewayAccessLog(ctx context.Context, on bool) error {
	return b.S.Gateway.SetAccessLog(ctx, on)
}

// SetGatewayListen implements api.Backend.
func (b Backend) SetGatewayListen(ctx context.Context, addrs []string) error {
	return b.S.Gateway.SetListen(ctx, addrs)
}

// AddGatewayListener implements api.Backend.
func (b Backend) AddGatewayListener(ctx context.Context, l api.Listener) error {
	return b.S.Gateway.AddListener(ctx, gateway.Listener{Name: l.Name, Port: l.Port, Protocol: l.Protocol})
}

// RemoveGatewayListener implements api.Backend.
func (b Backend) RemoveGatewayListener(ctx context.Context, name string) error {
	return b.S.Gateway.RemoveListener(ctx, name)
}

func wireResult(r *releases.Result) *api.Result {
	if r == nil {
		return nil
	}
	out := &api.Result{Release: r.Release, Revision: r.Revision, From: r.From, Status: r.Status, DryRun: r.DryRun, NoOp: r.NoOp,
		Plan: api.Plan{Changes: []api.Change{}}, Warnings: r.Warnings, Notes: r.Notes}
	for _, c := range r.Plan {
		out.Plan.Changes = append(out.Plan.Changes, api.Change{Kind: c.Kind, Name: c.Name, Action: c.Action, Fields: wireFields(c.Fields),
			Strategy: c.Strategy, StrategyReason: c.StrategyReason, Notice: c.Notice})
	}
	return out
}

func wireFields(fs []deployment.FieldChange) []api.FieldChange {
	return wireAll(fs, func(f deployment.FieldChange) api.FieldChange {
		return api.FieldChange{Path: f.Path, Old: f.Old, New: f.New}
	})
}

func wireRelease(r release.Release) api.Release {
	return api.Release{Name: r.Name, PackageName: r.PackageName, PackageVersion: r.PackageVersion,
		CurrentRevision: r.CurrentRevision, Status: string(r.Status), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Paused: r.Paused}
}

func wireRevision(r release.Revision) api.Revision {
	return api.Revision{Release: r.Release, Number: r.Number, PackageName: r.PackageName, PackageVersion: r.PackageVersion,
		PackageDigest: r.PackageDigest, ValuesDigest: r.ValuesDigest, ManifestDigest: r.ManifestDigest,
		Status: string(r.Status), Description: r.Description, CreatedAt: r.CreatedAt}
}

func wireStep(s release.Step) api.Step {
	return api.Step{Kind: s.Kind, Name: s.Name, Action: s.Action, Status: s.Status, Error: s.Error}
}

// wireAll maps a list; nil stays nil so empty JSON lists keep their meaning.
func wireAll[T, W any](in []T, f func(T) W) []W {
	if in == nil {
		return nil
	}
	out := make([]W, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
