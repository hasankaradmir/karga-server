package releases

import (
	"context"
	"errors"
	"fmt"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// ResumeOperation is the type of the operation that finishes one karga-server left running when it stopped.
const ResumeOperation = "resume"

// Resume finishes the operations a stopped karga-server left running. Install, upgrade and rollback
// deploy their revision again: the executor skips what is already done. Uninstall runs again, keeping
// volumes and history (its options are not recorded). Other operations are aborted; the reconciler
// starts what they stopped.
func (s *Service) Resume(ctx context.Context) {
	rels, err := s.Store.ListReleases(ctx)
	if err != nil {
		s.Log.Error("resume: list releases", "error", err)
		return
	}
	for _, rel := range rels {
		if err := s.resumeRelease(ctx, rel.Name); err != nil {
			s.Log.Error("resume interrupted operation", "release", rel.Name, "error", err)
		}
	}
}

// resumeRelease resumes the operation left running on release name, if any. Under the release's lock
// no operation of this process runs, so one recorded with this process's PID is an earlier
// process's that had the same PID.
func (s *Service) resumeRelease(ctx context.Context, name string) error {
	unlock, err := s.lock(ctx, name, false)
	if err != nil {
		return err
	}
	defer unlock()
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return err
	}
	op, err := s.Store.RunningOperation(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if op.Hostname == s.Hostname && op.PID != s.PID && processAlive(op.PID) {
		return nil // another karga still works on it
	}
	return s.resume(ctx, rel, op)
}

func (s *Service) resume(ctx context.Context, rel release.Release, op release.Operation) error {
	s.Log.Warn("resuming interrupted operation", "release", rel.Name, "operation", op.ID, "type", op.Type, "revision", op.Revision)
	// Deploys and uninstalls abort the interrupted operation when they begin (Force): if they cannot
	// begin (no container runtime yet), it stays running and the next start tries again.
	switch op.Type {
	case "install", "upgrade", "rollback", ResumeOperation:
		return s.redeploy(ctx, rel, op)
	case "uninstall":
		_, err := s.uninstallRelease(ctx, UninstallRequest{Name: rel.Name, KeepHistory: true, Force: true})
		return err
	}
	op.Status, op.Error = release.OperationAborted, "interrupted: karga-server stopped"
	return s.Store.FinishOperation(ctx, op)
}

// redeploy deploys the revision op was deploying. Without it (stopped before it was recorded) nothing
// had changed: the release goes back to its current revision.
func (s *Service) redeploy(ctx context.Context, rel release.Release, op release.Operation) error {
	rev, err := s.Store.GetRevision(ctx, rel.Name, op.Revision)
	if errors.Is(err, ports.ErrNotFound) {
		op.Status, op.Error = release.OperationAborted, "interrupted before it changed anything"
		if err := s.Store.FinishOperation(ctx, op); err != nil {
			return err
		}
		if rel.CurrentRevision == 0 {
			s.discard(ctx, rel.Name)
			return nil
		}
		rel.Status, rel.UpdatedAt = release.StatusDeployed, s.Now()
		return s.Store.SaveRelease(ctx, rel)
	}
	if err != nil {
		return err
	}
	rs, err := decodeManifest(rev.Manifest)
	if err == nil {
		err = s.restoreSecrets(rel.Name, rev.Number, rs)
	}
	if err != nil {
		// Cannot deploy it again: the release is left failed, for the next upgrade or rollback.
		rel.Status, rel.UpdatedAt = release.StatusFailed, s.Now()
		op.Status, op.Error = release.OperationAborted, "interrupted; cannot resume: "+err.Error()
		return errors.Join(err, s.Store.FinishOperation(ctx, op), s.Store.UpdateRevisionStatus(ctx, rel.Name, rev.Number, release.RevisionFailed, "Interrupted: "+err.Error()),
			s.Store.SaveRelease(ctx, rel))
	}
	p := packages.Metadata{Name: rev.PackageName, Version: rev.PackageVersion, Runtime: packages.Runtime{Supported: rev.RuntimeSupported}}
	_, err = s.deploy(ctx, deployInput{
		// The interrupted operation passed the volume check: its plan was allowed to delete volumes.
		CommonOptions: CommonOptions{DeleteVolumes: true, Force: true},
		Name:          rel.Name, Type: op.Type, Description: rev.Description, resumed: true,
		Package: p, PackageDigest: rev.PackageDigest, Overrides: rev.Values, Values: rev.ValuesAll,
		Resources: rs, Revision: rev.Number, Existing: &rel, Notes: rev.Notes,
	})
	if err != nil {
		return fmt.Errorf("revision %d: %w", rev.Number, err)
	}
	return nil
}
