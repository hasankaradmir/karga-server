package releases

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// ResumeOperation is the type of the operation that finishes one karga-server left running when it stopped.
const ResumeOperation = release.OpResume

// resumeRetry is the first pause before an operation is resumed again when the runtime is
// unavailable; it doubles up to resumeRetryMax.
var resumeRetry = 5 * time.Second

const resumeRetryMax = 5 * time.Minute

// Resume finishes the operations a stopped karga-server left running. Install, upgrade and rollback
// deploy their revision again: the executor skips what is already done. Uninstall runs again. Each
// resumes with the options its request gave (--delete-volumes, --keep-history, --strategy,
// --timeout), recorded with the operation. Other operations are aborted; the reconciler starts what
// they stopped.
//
// It takes the lock of every release it resumes before it returns, then resumes them one by one in
// the background: a command on one of them waits for its resume; commands on the others run at once.
// A resume the runtime is unavailable for (karga-server started before Docker) is tried again, with
// growing pauses, until ctx ends. The channel is closed when every resume has ended.
func (s *Lifecycle) Resume(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	type held struct {
		name   string
		unlock func()
	}
	var pending []held
	if rels, err := s.c.store.ListReleases(ctx); err != nil {
		s.c.log.Error("resume: list releases", "error", err)
	} else {
		s.closeOrphans(ctx, rels)
		for _, rel := range rels {
			if _, err := s.c.store.RunningOperation(ctx, rel.Name); err != nil {
				continue // nothing to resume (or it cannot be read: the command that meets it says so)
			}
			unlock, err := s.c.locks.lock(ctx, rel.Name)
			if err != nil {
				s.c.log.Error("resume interrupted operation", "release", rel.Name, "error", err)
				continue
			}
			pending = append(pending, held{rel.Name, unlock})
		}
	}
	go func() {
		defer close(done)
		for _, h := range pending {
			s.resumeHeld(ctx, h.name)
			h.unlock()
		}
	}()
	return done
}

// closeOrphans ends the operations left running on releases that are gone: an uninstall stopped
// after it removed its release had done its work; anything else is aborted. Without it, a new
// install of the name would take the release for interrupted.
func (s *Lifecycle) closeOrphans(ctx context.Context, rels []release.Release) {
	ops, err := s.c.store.ListOperations(ctx, "", 0, 1000)
	if err != nil {
		s.c.log.Error("resume: list operations", "error", err)
		return
	}
	for _, op := range ops {
		if op.Status != release.OperationRunning || slices.ContainsFunc(rels, func(r release.Release) bool { return r.Name == op.Release }) {
			continue
		}
		op.Status, op.Error = release.OperationAborted, "interrupted: karga-server stopped, and the release is gone"
		if op.Type == release.OpUninstall {
			op.Status, op.Error = release.OperationSucceeded, ""
		}
		if err := s.c.store.FinishOperation(ctx, op); err != nil {
			s.c.log.Error("resume: close the operation of a release that is gone", "release", op.Release, "error", err)
		}
	}
}

// resumeHeld resumes release name, whose lock the caller holds, once the runtime answers: a resume
// started without it would fail and leave the release failed instead of resumed.
func (s *Lifecycle) resumeHeld(ctx context.Context, name string) {
	for pause := resumeRetry; ; pause = min(2*pause, resumeRetryMax) {
		_, err := s.c.rt.Info(ctx)
		if !errors.Is(err, ports.ErrUnavailable) {
			// A resume that started finishes even when the server stops: ctx only ends the waiting.
			if err := s.resumeRelease(context.WithoutCancel(ctx), name); err != nil {
				s.c.log.Error("resume interrupted operation", "release", name, "error", err)
			}
			return
		}
		s.c.log.Warn("resume interrupted operation: the runtime is unavailable; trying again", "release", name, "in", pause, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// resumeRelease resumes the operation left running on release name, if any. The caller holds the
// release's lock, so no operation of this process runs on it: the operation is an earlier process's.
func (s *Lifecycle) resumeRelease(ctx context.Context, name string) error {
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
	return s.resume(ctx, rel, op)
}

func (s *Lifecycle) resume(ctx context.Context, rel release.Release, op release.Operation) error {
	s.c.log.Warn("resuming interrupted operation", "release", rel.Name, "operation", op.ID, "type", op.Type, "revision", op.Revision)
	// Deploys and uninstalls abort the interrupted operation when they begin (Force): if they cannot
	// begin (no container runtime yet), it stays running and the next start tries again.
	switch op.Type {
	case release.OpInstall, release.OpUpgrade, release.OpRollback, ResumeOperation:
		return s.redeploy(ctx, rel, op)
	case release.OpUninstall:
		_, err := s.uninstallRelease(ctx, UninstallRequest{Name: rel.Name, Force: true,
			KeepHistory: op.Options.KeepHistory, DeleteVolumes: op.Options.DeleteVolumes})
		return err
	case release.OpVolumeRestore:
		return s.resumeRestore(ctx, rel, op)
	}
	op.Status, op.Error = release.OperationAborted, "interrupted: karga-server stopped"
	return s.c.store.FinishOperation(ctx, op)
}

// resumeRestore finishes an interrupted volume restore from the backup it kept: stopped before it,
// the volume may be empty or half written. Without the backup the release is paused, so that the
// reconciler does not start its workloads on such a volume.
func (s *Lifecycle) resumeRestore(ctx context.Context, rel release.Release, op release.Operation) error {
	o := op.Options.Restore
	if o != nil {
		if _, err := os.Stat(o.From); err == nil {
			taken := slices.ContainsFunc(op.Steps, func(st release.Step) bool { return st.Action == restorePointStep })
			_, err := (&Volumes{s.c}).restore(ctx, restoreInput{Release: rel.Name, Volume: o.Volume, Name: o.Name, From: o.From, Force: true,
				Merge: o.Merge, NoRestorePoint: o.NoRestorePoint, Undo: o.Undo, pointTaken: taken}, emitter(nil), true)
			return err
		}
	}
	rel.Paused, rel.UpdatedAt = true, s.c.now()
	op.Status, op.Error = release.OperationAborted, "interrupted, and its backup is gone: the release is paused. "+
		"Restore the volume again (or 'karga volume undo-restore'), then 'karga resume'"
	return errors.Join(s.c.store.FinishOperation(ctx, op), s.c.store.SaveRelease(ctx, rel))
}

// redeploy deploys the revision op was deploying. Without it (stopped before it was recorded) nothing
// had changed: the release keeps the status it had, recorded with the revision it did not reach.
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
		if !rel.Status.InProgress() {
			return nil // a failed release stays failed: the reconciler leaves it to the user
		}
		rel.Status, rel.UpdatedAt = release.StatusDeployed, s.c.now() // its current revision runs
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
	p := pkgOf(rev)
	_, err = s.c.deploy(ctx, deployInput{
		// The request's own choices: a volume it was not allowed to delete is not deleted now either.
		CommonOptions: CommonOptions{DeleteVolumes: op.Options.DeleteVolumes, Timeout: op.Options.Timeout, Force: true,
			HistoryMax: op.Options.HistoryMax},
		Strategy: op.Options.Strategy,
		Name:     rel.Name, Type: op.Type, Description: rev.Description, resumed: true,
		Package: p, PackageDigest: rev.PackageDigest, Overrides: rev.Values, Values: rev.ValuesAll,
		Resources: rs, Revision: rev.Number, Existing: &rel, Notes: rev.Notes,
	})
	if err != nil {
		return fmt.Errorf("revision %d: %w", rev.Number, err)
	}
	return nil
}
