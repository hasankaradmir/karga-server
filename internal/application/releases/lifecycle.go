package releases

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func (s *core) getRelease(ctx context.Context, name string) (*release.Release, error) {
	rel, err := s.store.GetRelease(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

// render loads and renders a package for revision of release name.
func (s *core) render(ctx context.Context, name, path string, opts packages.Options) (deployInput, error) {
	revision, err := s.nextRevision(ctx, name)
	if err != nil {
		return deployInput{}, err
	}
	p, err := packages.Load(path)
	if err != nil {
		return deployInput{}, invalid{err}
	}
	opts.ReleaseName, opts.Revision = name, revision
	m, err := packages.Render(p, opts)
	if err != nil {
		return deployInput{}, invalid{err}
	}
	return deployInput{Name: name, Package: p.Metadata, PackageDigest: p.Digest, Overrides: m.Overrides,
		Values: m.Values, Resources: m.Resources(), Revision: revision, SecretPaths: m.SecretPaths, Notes: m.Notes}, nil
}

// valuesOptions are the render options for the values a client sent.
func valuesOptions(v Values) packages.Options {
	return packages.Options{Values: packages.ValueSources{Data: v.Files, Sets: v.Sets}, Strict: v.Strict}
}

// Install installs a package. Installing again with the same package and values is a no-op.
func (s *Lifecycle) Install(ctx context.Context, req InstallRequest, progress func(deployment.Event)) (*Result, error) {
	unlock, err := s.c.locks.lock(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.install(ctx, req, progress)
}

func (s *Lifecycle) install(ctx context.Context, req InstallRequest, progress func(deployment.Event)) (*Result, error) {
	existing, err := s.c.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	in, err := s.c.render(ctx, req.Name, req.Package, valuesOptions(req.Values))
	if err != nil {
		return nil, err
	}
	same := false
	if existing != nil && existing.Status == release.StatusDeployed {
		cur, err := s.c.store.GetRevision(ctx, req.Name, existing.CurrentRevision)
		if err != nil {
			return nil, err
		}
		valuesDigest, _, err := s.c.digestJSON(in.Values)
		if err != nil {
			return nil, err
		}
		same = cur.PackageDigest == in.PackageDigest && cur.ValuesDigest == valuesDigest
	}
	switch action, err := release.Install(existing, same); {
	case err != nil:
		return nil, err
	case action == release.InstallNoOp:
		cur := existing.CurrentRevision
		return &Result{Release: req.Name, Revision: cur, From: cur, Status: string(existing.Status), NoOp: true}, nil
	}
	in.CommonOptions, in.Type, in.Description, in.Existing, in.Progress = req.CommonOptions, release.OpInstall, "Install complete", existing, progress
	return s.c.deploy(ctx, in)
}

// Upgrade renders the package with new values and applies the difference.
func (s *Lifecycle) Upgrade(ctx context.Context, req UpgradeRequest, progress func(deployment.Event)) (*Result, error) {
	unlock, err := s.c.locks.lock(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.c.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		if req.Install {
			return s.install(ctx, InstallRequest{CommonOptions: req.CommonOptions, Name: req.Name, Package: req.Package, Values: req.Values}, progress)
		}
		return nil, fmt.Errorf("release %s: %w (use --install to create it)", req.Name, ports.ErrNotFound)
	}
	if err := existing.CanUpgrade(); err != nil {
		return nil, err
	}
	opts := valuesOptions(req.Values)
	if req.ReuseValues && existing.CurrentRevision > 0 {
		cur, err := s.c.store.GetRevision(ctx, req.Name, existing.CurrentRevision)
		if err != nil {
			return nil, err
		}
		// writeOnly values were never stored: they must be given again.
		hidden := redactedPaths(cur.Values)
		if len(hidden) > 0 {
			given, err := packages.Overrides(opts.Values, nil)
			if err != nil {
				return nil, err
			}
			for _, p := range hidden {
				if !hasValue(given, p) {
					return nil, fmt.Errorf("%s is writeOnly and was not stored; pass it again with --set %s=<value>", strings.Join(p, "."), strings.Join(p, "."))
				}
			}
		}
		opts.Values.Base = dropValues(cur.Values, hidden)
	}
	in, err := s.c.render(ctx, req.Name, req.Package, opts)
	if err != nil {
		return nil, err
	}
	in.CommonOptions, in.Type, in.Description, in.Existing = req.CommonOptions, release.OpUpgrade, "Upgrade complete", existing
	in.Strategy, in.Progress = req.Strategy, progress
	return s.c.deploy(ctx, in)
}

// Rollback re-applies the desired state of an earlier revision as a new revision.
func (s *Lifecycle) Rollback(ctx context.Context, req RollbackRequest, progress func(deployment.Event)) (*Result, error) {
	unlock, err := s.c.locks.lock(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.c.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("release %s: %w", req.Name, ports.ErrNotFound)
	}
	if err := existing.CanRollback(); err != nil {
		return nil, invalid{err}
	}
	target := req.Revision
	if target == 0 {
		if target, err = s.rollbackTarget(ctx, existing); err != nil {
			return nil, err
		}
	}
	rev, err := s.c.store.GetRevision(ctx, req.Name, target)
	if err != nil {
		return nil, err
	}
	if rev.Status != release.RevisionDeployed {
		return nil, fmt.Errorf("revision %d of %s is %s; only deployed revisions can be rolled back to", target, req.Name, rev.Status)
	}
	rs, err := decodeManifest(rev.Manifest)
	if err != nil {
		return nil, err
	}
	if err := s.c.restoreSecrets(req.Name, target, rs); err != nil {
		return nil, err
	}
	number, err := s.c.nextRevision(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	p := pkgOf(rev)
	return s.c.deploy(ctx, deployInput{
		CommonOptions: req.CommonOptions, Progress: progress, Name: req.Name, Type: release.OpRollback, Description: fmt.Sprintf("Rollback to %d", target),
		Package: p, PackageDigest: rev.PackageDigest, Overrides: rev.Values, Values: rev.ValuesAll,
		Resources: rs, Revision: number, Existing: existing, Strategy: req.Strategy, Notes: rev.Notes,
	})
}

// rollbackTarget picks the revision to return to: after a failure the last good one (the current
// revision), otherwise the last deployed revision before the current one.
func (s *Lifecycle) rollbackTarget(ctx context.Context, rel *release.Release) (int, error) {
	if rel.Status == release.StatusFailed && rel.CurrentRevision > 0 {
		return rel.CurrentRevision, nil
	}
	revs, err := s.c.store.ListRevisions(ctx, rel.Name)
	if err != nil {
		return 0, err
	}
	for i := len(revs) - 1; i >= 0; i-- {
		if revs[i].Number < rel.CurrentRevision && revs[i].Status == release.RevisionDeployed {
			return revs[i].Number, nil
		}
	}
	return 0, fmt.Errorf("release %s has no earlier deployed revision to roll back to", rel.Name)
}

// Uninstall removes the release's workloads and networks; volumes are kept unless DeleteVolumes or
// retention: delete. With KeepHistory the revisions stay and the release is marked uninstalled.
func (s *Lifecycle) Uninstall(ctx context.Context, req UninstallRequest) (*Result, error) {
	unlock, err := s.c.locks.lock(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.uninstallRelease(ctx, req)
}

func (s *Lifecycle) uninstallRelease(ctx context.Context, req UninstallRequest) (*Result, error) {
	existing, err := s.c.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("release %s: %w", req.Name, ports.ErrNotFound)
	}
	if existing.Status == release.StatusUninstalled {
		return nil, fmt.Errorf("%w: release %s is already uninstalled", ports.ErrConflict, req.Name)
	}
	rt := s.c.rt
	res := &Result{Release: req.Name, Revision: existing.CurrentRevision, From: existing.CurrentRevision, DryRun: req.DryRun}
	if req.DryRun {
		res.Status = string(existing.Status)
		res.Plan, err = s.c.env.DeletePlan(ctx, rt, req.Name, req.DeleteVolumes)
		return res, err
	}
	op, err := s.c.begin(ctx, req.Name, release.OpUninstall, existing.CurrentRevision, req.Force,
		release.Options{DeleteVolumes: req.DeleteVolumes, KeepHistory: req.KeepHistory})
	if err != nil {
		return nil, err
	}
	err = s.uninstall(ctx, rt, *existing, req)
	s.c.finish(ctx, op, err)
	if err != nil {
		return nil, err
	}
	res.Status = string(release.StatusUninstalled)
	return res, nil
}

func (s *Lifecycle) uninstall(ctx context.Context, rt ports.Runtime, rel release.Release, req UninstallRequest) error {
	if rel.Status != release.StatusDeployed && rel.Status != release.StatusFailed {
		rel.Status = release.StatusFailed // interrupted operation that begin just aborted
	}
	if err := release.Transition(rel.Status, release.StatusUninstalling); err != nil {
		return err
	}
	rel.Status, rel.UpdatedAt = release.StatusUninstalling, s.c.now()
	if err := s.c.store.SaveRelease(ctx, rel); err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)
	if err := s.c.env.Delete(ctx, rt, req.Name, req.DeleteVolumes); err != nil {
		rel.Status = release.StatusFailed
		return errors.Join(err, s.c.store.SaveRelease(bg, rel))
	}
	// The release is gone: a gateway that cannot drop its routes now does at its next sync.
	if err := s.c.env.SyncGateway(ctx, rt); err != nil {
		s.c.log.Warn("uninstall: sync the gateway", "release", req.Name, "error", err)
	}
	s.c.repairs.reset(req.Name) // what was repaired, or given up on, is no more
	if req.DeleteVolumes {
		// The volumes' restore points go with them.
		if err := os.RemoveAll(filepath.Join(s.c.home, "restore-points", req.Name)); err != nil {
			return err
		}
	}
	if !req.KeepHistory {
		return s.c.store.DeleteRelease(bg, req.Name)
	}
	rel.Status, rel.UpdatedAt = release.StatusUninstalled, s.c.now()
	return s.c.store.InTx(bg, func(st ports.StateStore) error {
		applied, err := st.ListResources(bg, req.Name)
		if err != nil {
			return err
		}
		for _, a := range applied {
			if err := st.DeleteResource(bg, req.Name, a.Kind, a.Name); err != nil {
				return err
			}
		}
		return st.SaveRelease(bg, rel)
	})
}
