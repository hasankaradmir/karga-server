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
// deploy their revision again: the executor skips what is already done. Uninstall runs again. Each
// resumes with the options its request gave (--delete-volumes, --keep-history, --strategy,
// --timeout), recorded with the operation. Other operations are aborted; the reconciler starts what
// they stopped.
func (s *Lifecycle) Resume(ctx context.Context) {
	rels, err := s.c.store.ListReleases(ctx)
	if err != nil {
		s.c.log.Error("resume: list releases", "error", err)
		return
	}
	for _, rel := range rels {
		if err := s.resumeRelease(ctx, rel.Name); err != nil {
			s.c.log.Error("resume interrupted operation", "release", rel.Name, "error", err)
		}
	}
}

// resumeRelease resumes the operation left running on release name, if any. Under the release's lock
// no operation of this process runs, so one recorded with this process's PID is an earlier
// process's that had the same PID.
func (s *Lifecycle) resumeRelease(ctx context.Context, name string) error {
	unlock, err := s.c.locks.lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	rel, err := s.c.store.GetRelease(ctx, name)
	if err != nil {
		return err
	}
	op, err := s.c.store.RunningOperation(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if op.Hostname == s.c.hostname && op.PID != s.c.pid && processAlive(op.PID) {
		return nil // a live process still works on it
	}
	return s.resume(ctx, rel, op)
}

func (s *Lifecycle) resume(ctx context.Context, rel release.Release, op release.Operation) error {
	s.c.log.Warn("resuming interrupted operation", "release", rel.Name, "operation", op.ID, "type", op.Type, "revision", op.Revision)
	// Deploys and uninstalls abort the interrupted operation when they begin (Force): if they cannot
	// begin (no container runtime yet), it stays running and the next start tries again.
	switch op.Type {
	case "install", "upgrade", "rollback", ResumeOperation:
		return s.redeploy(ctx, rel, op)
	case "uninstall":
		_, err := s.uninstallRelease(ctx, UninstallRequest{Name: rel.Name, Force: true,
			KeepHistory: op.Options.KeepHistory, DeleteVolumes: op.Options.DeleteVolumes})
		return err
	}
	op.Status, op.Error = release.OperationAborted, "interrupted: karga-server stopped"
	return s.c.store.FinishOperation(ctx, op)
}

// redeploy deploys the revision op was deploying. Without it (stopped before it was recorded) nothing
// had changed: the release goes back to its current revision.
func (s *Lifecycle) redeploy(ctx context.Context, rel release.Release, op release.Operation) error {
	rev, err := s.c.store.GetRevision(ctx, rel.Name, op.Revision)
	if errors.Is(err, ports.ErrNotFound) {
		op.Status, op.Error = release.OperationAborted, "interrupted before it changed anything"
		if err := s.c.store.FinishOperation(ctx, op); err != nil {
			return err
		}
		if rel.CurrentRevision == 0 {
			s.c.discard(ctx, rel.Name)
			return nil
		}
		rel.Status, rel.UpdatedAt = release.StatusDeployed, s.c.now()
		return s.c.store.SaveRelease(ctx, rel)
	}
	if err != nil {
		return err
	}
	rs, err := decodeManifest(rev.Manifest)
	if err == nil {
		err = s.c.restoreSecrets(rel.Name, rev.Number, rs)
	}
	if err != nil {
		// Cannot deploy it again: the release is left failed, for the next upgrade or rollback.
		rel.Status, rel.UpdatedAt = release.StatusFailed, s.c.now()
		op.Status, op.Error = release.OperationAborted, "interrupted; cannot resume: "+err.Error()
		return errors.Join(err, s.c.store.FinishOperation(ctx, op), s.c.store.UpdateRevisionStatus(ctx, rel.Name, rev.Number, release.RevisionFailed, "Interrupted: "+err.Error()),
			s.c.store.SaveRelease(ctx, rel))
	}
	p := packages.Metadata{Name: rev.PackageName, Version: rev.PackageVersion, Runtime: packages.Runtime{Supported: rev.RuntimeSupported}}
	_, err = s.c.deploy(ctx, deployInput{
		// The request's own choices: a volume it was not allowed to delete is not deleted now either.
		CommonOptions: CommonOptions{DeleteVolumes: op.Options.DeleteVolumes, Timeout: op.Options.Timeout, Force: true},
		Strategy:      op.Options.Strategy,
		Name:          rel.Name, Type: op.Type, Description: rev.Description, resumed: true,
		Package: p, PackageDigest: rev.PackageDigest, Overrides: rev.Values, Values: rev.ValuesAll,
		Resources: rs, Revision: rev.Number, Existing: &rel, Notes: rev.Notes,
	})
	if err != nil {
		return fmt.Errorf("revision %d: %w", rev.Number, err)
	}
	return nil
}
