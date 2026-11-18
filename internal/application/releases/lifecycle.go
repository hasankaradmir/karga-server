package releases

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// InstallRequest is shared with the CLI (karga-sdk/api).
type InstallRequest = api.InstallRequest

// UpgradeRequest is shared with the CLI (karga-sdk/api).
type UpgradeRequest = api.UpgradeRequest

// RollbackRequest is shared with the CLI (karga-sdk/api).
type RollbackRequest = api.RollbackRequest

// UninstallRequest is shared with the CLI (karga-sdk/api).
type UninstallRequest = api.UninstallRequest

func (s *Service) getRelease(ctx context.Context, name string) (*release.Release, error) {
	rel, err := s.Store.GetRelease(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

// render loads and renders a package for revision of release name.
func (s *Service) render(ctx context.Context, name, path string, opts packages.Options) (deployInput, error) {
	revision, err := s.nextRevision(ctx, name)
	if err != nil {
		return deployInput{}, err
	}
	p, err := packages.Load(path)
	if err != nil {
		return deployInput{}, invalid{err}
	}
	digest, err := packages.Digest(path)
	if err != nil {
		return deployInput{}, err
	}
	opts.ReleaseName, opts.Revision = name, revision
	m, err := packages.Render(p, opts)
	if err != nil {
		return deployInput{}, invalid{err}
	}
	return deployInput{Name: name, Package: p.Metadata, PackageDigest: digest, Overrides: m.Overrides,
		Values: m.Values, Resources: m.Resources(), Revision: revision, SecretPaths: m.SecretPaths, Notes: m.Notes}, nil
}

// valuesOptions are the render options for the values a client sent.
func valuesOptions(v api.ValuesInput) packages.Options {
	return packages.Options{ValueData: v.Files, Sets: v.Set, SetStrings: v.SetString, Strict: v.Strict}
}

// Install installs a package. Installing again with the same package and values is a no-op.
func (s *Service) Install(ctx context.Context, req InstallRequest, progress func(api.Event)) (*Result, error) {
	unlock, err := s.lock(ctx, req.Name, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.install(ctx, req, progress)
}

func (s *Service) install(ctx context.Context, req InstallRequest, progress func(api.Event)) (*Result, error) {
	existing, err := s.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	in, err := s.render(ctx, req.Name, req.Package, valuesOptions(req.Values))
	if err != nil {
		return nil, err
	}
	if existing != nil {
		switch {
		case existing.Status == release.StatusDeployed:
			cur, err := s.Store.GetRevision(ctx, req.Name, existing.CurrentRevision)
			if err != nil {
				return nil, err
			}
			if err := s.loadKey(); err != nil {
				return nil, err
			}
			valuesDigest, _, err := digestJSON(in.Values)
			if err != nil {
				return nil, err
			}
			if cur.PackageDigest == in.PackageDigest && cur.ValuesDigest == valuesDigest {
				return &Result{Release: req.Name, Revision: cur.Number, From: cur.Number, Status: string(existing.Status), NoOp: true}, nil
			}
			return nil, fmt.Errorf("%w: %s is deployed with a different package or values; use 'karga upgrade'", ErrExists, req.Name)
		case existing.Status == release.StatusFailed && existing.CurrentRevision > 0:
			return nil, fmt.Errorf("%w: %s failed after revision %d; use 'karga upgrade' or 'karga rollback'", ErrExists, req.Name, existing.CurrentRevision)
		}
		// A never-deployed failed release, an uninstalled one or an interrupted one is retried.
	}
	in.CommonOptions, in.Type, in.Description, in.Existing, in.Progress = req.CommonOptions, "install", "Install complete", existing, progress
	return s.deploy(ctx, in)
}

// Upgrade renders the package with new values and applies the difference.
func (s *Service) Upgrade(ctx context.Context, req UpgradeRequest, progress func(api.Event)) (*Result, error) {
	unlock, err := s.lock(ctx, req.Name, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		if req.Install {
			return s.install(ctx, InstallRequest{CommonOptions: req.CommonOptions, Name: req.Name, Package: req.Package, Values: req.Values}, progress)
		}
		return nil, fmt.Errorf("release %s: %w (use --install to create it)", req.Name, ports.ErrNotFound)
	}
	if existing.Status == release.StatusUninstalled {
		return nil, fmt.Errorf("release %s is uninstalled; use 'karga install'", req.Name)
	}
	opts := valuesOptions(req.Values)
	if req.ReuseValues && existing.CurrentRevision > 0 {
		cur, err := s.Store.GetRevision(ctx, req.Name, existing.CurrentRevision)
		if err != nil {
			return nil, err
		}
		// writeOnly values were never stored: they must be given again.
		hidden := redactedPaths(cur.Values)
		if len(hidden) > 0 {
			given, err := packages.Overrides(opts)
			if err != nil {
				return nil, err
			}
			for _, p := range hidden {
				if !hasValue(given, p) {
					return nil, fmt.Errorf("%s is writeOnly and was not stored; pass it again with --set %s=<value>", strings.Join(p, "."), strings.Join(p, "."))
				}
			}
		}
		opts.BaseOverrides = dropValues(cur.Values, hidden)
	}
	in, err := s.render(ctx, req.Name, req.Package, opts)
	if err != nil {
		return nil, err
	}
	in.CommonOptions, in.Type, in.Description, in.Existing = req.CommonOptions, "upgrade", "Upgrade complete", existing
	in.Strategy, in.Progress = req.Strategy, progress
	return s.deploy(ctx, in)
}

// Rollback re-applies the desired state of an earlier revision as a new revision.
func (s *Service) Rollback(ctx context.Context, req RollbackRequest, progress func(api.Event)) (*Result, error) {
	unlock, err := s.lock(ctx, req.Name, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := s.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("release %s: %w", req.Name, ports.ErrNotFound)
	}
	target := req.Revision
	if target == 0 {
		if target, err = s.rollbackTarget(ctx, existing); err != nil {
			return nil, err
		}
	}
	rev, err := s.Store.GetRevision(ctx, req.Name, target)
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
	if err := s.restoreSecrets(req.Name, target, rs); err != nil {
		return nil, err
	}
	number, err := s.nextRevision(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	p := packages.Metadata{Name: rev.PackageName, Version: rev.PackageVersion, Runtime: packages.Runtime{Supported: rev.RuntimeSupported}}
	return s.deploy(ctx, deployInput{
		CommonOptions: req.CommonOptions, Progress: progress, Name: req.Name, Type: "rollback", Description: fmt.Sprintf("Rollback to %d", target),
		Package: p, PackageDigest: rev.PackageDigest, Overrides: rev.Values, Values: rev.ValuesAll,
		Resources: rs, Revision: number, Existing: existing, Strategy: req.Strategy, Notes: rev.Notes,
	})
}

// rollbackTarget picks the revision to return to: after a failure the last good one (the current
// revision), otherwise the last deployed revision before the current one.
func (s *Service) rollbackTarget(ctx context.Context, rel *release.Release) (int, error) {
	if rel.Status == release.StatusFailed && rel.CurrentRevision > 0 {
		return rel.CurrentRevision, nil
	}
	revs, err := s.Store.ListRevisions(ctx, rel.Name)
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
func (s *Service) Uninstall(ctx context.Context, req UninstallRequest) (*Result, error) {
	unlock, err := s.lock(ctx, req.Name, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.uninstallRelease(ctx, req)
}

func (s *Service) uninstallRelease(ctx context.Context, req UninstallRequest) (*Result, error) {
	existing, err := s.getRelease(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("release %s: %w", req.Name, ports.ErrNotFound)
	}
	if existing.Status == release.StatusUninstalled {
		return nil, fmt.Errorf("%w: release %s is already uninstalled", api.ErrConflict, req.Name)
	}
	rt, err := s.runtime(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{Release: req.Name, Revision: existing.CurrentRevision, From: existing.CurrentRevision, DryRun: req.DryRun}
	if req.DryRun {
		res.Status = string(existing.Status)
		res.Plan, err = deployment.DeletePlan(ctx, rt, req.Name, req.DeleteVolumes)
		return res, err
	}
	op, err := s.begin(ctx, req.Name, "uninstall", existing.CurrentRevision, req.Force,
		release.Options{DeleteVolumes: req.DeleteVolumes, KeepHistory: req.KeepHistory})
	if err != nil {
		return nil, err
	}
	err = s.uninstall(ctx, rt, *existing, req)
	s.finish(ctx, op, err)
	if err != nil {
		return nil, err
	}
	res.Status = string(release.StatusUninstalled)
	return res, nil
}

func (s *Service) uninstall(ctx context.Context, rt ports.Runtime, rel release.Release, req UninstallRequest) error {
	if rel.Status != release.StatusDeployed && rel.Status != release.StatusFailed {
		rel.Status = release.StatusFailed // interrupted operation that begin just aborted
	}
	if err := release.Transition(rel.Status, release.StatusUninstalling); err != nil {
		return err
	}
	rel.Status, rel.UpdatedAt = release.StatusUninstalling, s.Now()
	if err := s.Store.SaveRelease(ctx, rel); err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)
	if err := deployment.Delete(ctx, rt, s.Home, req.Name, req.DeleteVolumes); err != nil {
		rel.Status = release.StatusFailed
		return errors.Join(err, s.Store.SaveRelease(bg, rel))
	}
	if !req.KeepHistory {
		return s.Store.DeleteRelease(bg, req.Name)
	}
	rel.Status, rel.UpdatedAt = release.StatusUninstalled, s.Now()
	return s.Store.InTx(bg, func(st ports.StateStore) error {
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
