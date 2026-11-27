package releases

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

type mounter struct {
	name, runtime, path string
	readOnly            bool
}

// mounters lists the workloads of a release that mount volume.
func (s *Volumes) mounters(ctx context.Context, rel, volume string) ([]mounter, error) {
	applied, err := s.c.store.ListResources(ctx, rel)
	if err != nil {
		return nil, err
	}
	var out []mounter
	known := false
	for _, a := range applied {
		known = known || (a.Kind == resource.KindVolume && a.Name == volume)
		if a.Kind != resource.KindWorkload {
			continue
		}
		r, err := resource.DecodeStored(a.Spec)
		if err != nil {
			return nil, err
		}
		for _, m := range r.Spec.(*resource.WorkloadSpec).Mounts {
			if m.Volume == volume {
				out = append(out, mounter{a.Name, a.RuntimeName, m.MountPath, m.ReadOnly})
			}
		}
	}
	switch {
	case !known:
		return nil, fmt.Errorf("release %s has no volume %q: %w", rel, volume, ports.ErrNotFound)
	case len(out) == 0:
		return nil, fmt.Errorf("no workload of release %s mounts volume %q, so it cannot be copied", rel, volume)
	}
	return out, nil
}

// scope is what a volume operation stops and starts again: workloads in start order (those they
// depend on first), with their containers and specs.
type scope struct {
	order     []string
	container map[string]string
	spec      map[string]*resource.WorkloadSpec
}

// scopeOf is the mounters (only those writing to the volume, with writers) and every workload
// that depends on them through dependsOn. The dependencies come from the current revision's
// manifest (the applied specs leave dependsOn out); containers and specs from what is applied.
func (s *Volumes) scopeOf(ctx context.Context, rel string, ms []mounter, writers bool) (scope, error) {
	r, err := s.c.store.GetRelease(ctx, rel)
	if err != nil {
		return scope{}, err
	}
	rev, err := s.c.store.GetRevision(ctx, rel, r.CurrentRevision)
	if err != nil {
		return scope{}, err
	}
	rs, err := decodeManifest(rev.Manifest)
	if err != nil {
		return scope{}, err
	}
	applied, err := s.c.store.ListResources(ctx, rel)
	if err != nil {
		return scope{}, err
	}
	sc := scope{container: map[string]string{}, spec: map[string]*resource.WorkloadSpec{}}
	for _, a := range applied {
		if a.Kind != resource.KindWorkload {
			continue
		}
		r, err := resource.DecodeStored(a.Spec)
		if err != nil {
			return scope{}, err
		}
		sc.container[a.Name], sc.spec[a.Name] = a.RuntimeName, r.Spec.(*resource.WorkloadSpec)
	}
	var names []string
	for _, m := range ms {
		if !writers || !m.readOnly {
			names = append(names, m.name)
		}
	}
	order, err := deployment.WithDependents(rs, names)
	for _, n := range order {
		if _, ok := sc.container[n]; ok { // applied: it has a container to stop
			sc.order = append(sc.order, n)
		}
	}
	return sc, err
}

// stop stops the running workloads of sc, dependents first, and returns those it stopped in start
// order.
func stop(ctx context.Context, rt ports.Runtime, sc scope, emit func(kind, name, phase, detail string)) ([]string, error) {
	var stopped []string
	for _, n := range slices.Backward(sc.order) {
		st, err := rt.GetWorkload(ctx, sc.container[n])
		if errors.Is(err, ports.ErrNotFound) {
			continue
		}
		if err != nil {
			return stopped, err
		}
		if st.State != ports.StateRunning {
			continue
		}
		emit(resource.KindWorkload, n, deployment.PhaseStop, "stopping "+sc.container[n])
		if err := rt.StopWorkload(ctx, sc.container[n], 0); err != nil {
			return stopped, err
		}
		stopped = slices.Insert(stopped, 0, n)
	}
	return stopped, nil
}

// start starts the workloads stopped, in order, each once it is ready: those after it may need it.
// One that does not get ready fails it with ErrNotReady; the others are started all the same.
func (s *Volumes) start(ctx context.Context, rt ports.Runtime, sc scope, stopped []string, emit func(kind, name, phase, detail string)) error {
	var errs []error
	for _, n := range stopped {
		emit(resource.KindWorkload, n, deployment.PhaseStart, "starting "+sc.container[n])
		err := rt.StartWorkload(context.WithoutCancel(ctx), sc.container[n])
		if err == nil {
			// The wait reports what it waits for, as a deployment's does.
			err = s.c.env.WaitReady(deployment.WithProgress(ctx, toEvent(emit)), rt, map[string]*resource.WorkloadSpec{sc.container[n]: sc.spec[n]}, 0)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("workload %s did not get ready: %w: %w", n, ErrNotReady, err))
			continue
		}
		emit(resource.KindWorkload, n, deployment.PhaseDone, "start") // the CLI prints "start done"
	}
	return errors.Join(errs...)
}

// emitter sends progress events to fn (nil: none).
func emitter(fn func(deployment.Event)) func(kind, name, phase, detail string) {
	return func(kind, name, phase, detail string) {
		if fn != nil {
			fn(deployment.Event{Kind: kind, Name: name, Phase: phase, Detail: detail})
		}
	}
}

// Backup writes req.Volume to w. A live copy only reads: it holds nothing, so a slow client never
// blocks the release. Otherwise the workloads writing to the volume, and those that depend on
// them, are stopped, and the release held as an operation, only while the copy is made: it goes
// to a temporary file under $KARGA_HOME/tmp, the workloads run again, ready, and the file is sent
// after. The file is sent when they do not get ready too (ErrNotReady): the copy is complete.
func (s *Volumes) Backup(ctx context.Context, req VolumeBackupRequest, w io.Writer, progress func(deployment.Event)) (*VolumeResult, error) {
	res := &VolumeResult{}
	if req.Live {
		ms, err := s.mounters(ctx, req.Release, req.Volume)
		if err != nil {
			return nil, err
		}
		s.c.log.Info("volume backup", "release", req.Release, "volume", req.Volume, "user", actorOf(ctx))
		return res, s.write(ctx, req, ms[0], w)
	}
	dir := filepath.Join(s.c.home, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "karga-backup-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	err = s.stopped(ctx, req, f, res, emitter(progress))
	if err != nil && !errors.Is(err, ErrNotReady) {
		return nil, err
	}
	if _, serr := f.Seek(0, io.SeekStart); serr != nil {
		return nil, serr
	}
	if _, cerr := io.Copy(w, f); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return res, fmt.Errorf("volume %s of %s copied, but %w", req.Volume, req.Release, err)
	}
	return res, nil
}

// stopped copies req.Volume to w with the workloads writing to it, and those that depend on them,
// stopped, as an operation holding the release; they run again before it returns.
func (s *Volumes) stopped(ctx context.Context, req VolumeBackupRequest, w io.Writer, res *VolumeResult, emit func(kind, name, phase, detail string)) (err error) {
	unlock, err := s.c.locks.lock(ctx, req.Release)
	if err != nil {
		return err
	}
	defer unlock()
	ms, err := s.mounters(ctx, req.Release, req.Volume)
	if err != nil {
		return err
	}
	sc, err := s.scopeOf(ctx, req.Release, ms, true)
	if err != nil {
		return err
	}
	rel, err := s.c.store.GetRelease(ctx, req.Release)
	if err != nil {
		return err
	}
	op, err := s.c.begin(ctx, req.Release, release.OpVolumeBackup, rel.CurrentRevision, false, release.Options{})
	if err != nil {
		return err
	}
	defer func() { op.Downtime = res.Downtime; s.c.finish(ctx, op, err) }()
	rt := s.c.rt
	began := s.c.now()
	res.Stopped, err = stop(ctx, rt, sc, emit)
	if err == nil {
		emit(resource.KindVolume, req.Volume, deployment.PhaseCreate, "copying")
		err = s.write(ctx, req, ms[0], w)
	}
	// The workloads run again also when the client went away mid-copy (Ctrl-C): that stops the copy only.
	err = errors.Join(err, s.start(context.WithoutCancel(ctx), rt, sc, res.Stopped, emit))
	if len(res.Stopped) > 0 {
		res.Downtime = s.c.now().Sub(began)
	}
	return err
}

// restoreInput is one restore: of an uploaded backup, of a restore point (undo), or of either again
// after an interruption.
type restoreInput struct {
	Release, Volume, Name string
	// From is the backup file under KARGA_HOME: removed when the restore ends, unless Undo.
	From                         string
	Force, Merge, NoRestorePoint bool
	Undo                         bool
	// pointTaken: an interrupted restore had kept the restore point already; it is not taken again
	// from a volume that may be half written.
	pointTaken bool
}

// Restore replaces req.Volume's files with the backup read from r (Merge writes it over them). The
// workloads mounting it, and those that depend on them, are stopped meanwhile and start again, in
// order and ready, before it returns. The volume's files are kept as its restore point first
// (UndoRestore puts them back) unless NoRestorePoint. The backup stays under KARGA_HOME until the
// restore ends: an interrupted one is finished when karga-server starts again.
func (s *Volumes) Restore(ctx context.Context, req VolumeRestoreRequest, r io.Reader, progress func(deployment.Event)) (*VolumeResult, error) {
	from, err := s.keep(r)
	if err != nil {
		return nil, err
	}
	unlock, err := s.c.locks.lock(ctx, req.Release)
	if err != nil {
		_ = os.Remove(from)
		return nil, err
	}
	defer unlock()
	return s.restore(ctx, restoreInput{Release: req.Release, Volume: req.Volume, Name: cmp.Or(req.Name, "the backup"), From: from,
		Force: req.Force, Merge: req.Merge, NoRestorePoint: req.NoRestorePoint}, emitter(progress), false)
}

// UndoRestore puts back the files req.Volume had before its last restore, the way a restore does.
// It keeps no restore point of its own, and the restore point stays.
func (s *Volumes) UndoRestore(ctx context.Context, release, volume string, progress func(deployment.Event)) (*VolumeResult, error) {
	unlock, err := s.c.locks.lock(ctx, release)
	if err != nil {
		return nil, err
	}
	defer unlock()
	from := restorePointPath(s.c.home, release, volume)
	if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("volume %s of %s has no restore point (a restore keeps one): %w", volume, release, ports.ErrNotFound)
	}
	res, err := s.restore(ctx, restoreInput{Release: release, Volume: volume, Name: "its restore point", From: from, Force: true,
		NoRestorePoint: true, Undo: true}, emitter(progress), false)
	if res != nil {
		res.RestorePoint = from
	}
	return res, err
}

// keep copies an uploaded backup to KARGA_HOME/restores, so a restore can be finished from it.
func (s *Volumes) keep(r io.Reader) (string, error) {
	dir := filepath.Join(s.c.home, "restores")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "restore-*.tar.gz")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, r)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("keeping the backup: %w", err)
	}
	return f.Name(), nil
}

// restore runs one restore of a release the caller holds; resumed finishes one an earlier process
// left running.
func (s *Volumes) restore(ctx context.Context, in restoreInput, emit func(kind, name, phase, detail string), resumed bool) (res *VolumeResult, err error) {
	res = &VolumeResult{}
	if !in.Undo {
		defer func() { _ = os.Remove(in.From) }() // a crash leaves it for the resume
	}
	ms, err := s.mounters(ctx, in.Release, in.Volume)
	if err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(ctx, in.Release, ms, false)
	if err != nil {
		return nil, err
	}
	dst := slices.IndexFunc(ms, func(m mounter) bool { return !m.readOnly })
	if dst < 0 {
		return nil, fmt.Errorf("every workload mounts volume %q read-only; nothing can write to it", in.Volume)
	}
	// Everything that can be refused is, before anything changes.
	if err := s.checkBackup(in); err != nil {
		return nil, err
	}
	rel, err := s.c.store.GetRelease(ctx, in.Release)
	if err != nil {
		return nil, err
	}
	var vol ports.VolumeRequest
	var recreate []deployment.Mounter
	if !in.Merge {
		if vol, recreate, err = s.recreatePlan(ctx, in.Release, in.Volume); err != nil {
			return nil, err
		}
	}
	point := !in.NoRestorePoint && !in.pointTaken
	if point {
		if err := s.roomForRestorePoint(in.From); err != nil {
			return nil, err
		}
	}
	op, err := s.c.begin(ctx, in.Release, release.OpVolumeRestore, rel.CurrentRevision, resumed, release.Options{Restore: &release.RestoreOptions{
		Volume: in.Volume, From: in.From, Name: in.Name, Merge: in.Merge, NoRestorePoint: in.NoRestorePoint, Undo: in.Undo}})
	if err != nil {
		return nil, err
	}
	defer func() { op.Downtime = res.Downtime; s.c.finish(ctx, op, err) }()
	rt := s.c.rt
	began := s.c.now()
	defer func() {
		if errors.Is(err, errWrite) {
			err = s.keepStopped(ctx, rel, in, res, err)
			return
		}
		err = errors.Join(err, s.start(ctx, rt, sc, res.Stopped, emit))
		if len(res.Stopped) > 0 {
			res.Downtime = s.c.now().Sub(began)
		}
		if errors.Is(err, ErrNotReady) {
			err = fmt.Errorf("volume %s of %s restored, but %w", in.Volume, in.Release, err)
		}
	}()
	if res.Stopped, err = stop(ctx, rt, sc, emit); err != nil {
		return res, err
	}
	if resumed {
		// The interrupted restore had stopped them already: they are all started again, in order.
		res.Stopped = sc.order
	}
	if point {
		emit(resource.KindVolume, in.Volume, deployment.PhaseCreate, "keeping its files as a restore point")
		if res.RestorePoint, err = s.saveRestorePoint(ctx, in, ms[0]); err != nil {
			return res, err
		}
		op.Steps = append(op.Steps, release.Step{Kind: resource.KindVolume, Name: in.Volume, Action: restorePointStep, Status: deployment.StepDone})
		if err := s.c.store.SaveOperationSteps(ctx, op.Release, op.ID, op.Steps); err != nil {
			return res, err
		}
	} else if in.pointTaken {
		res.RestorePoint = restorePointPath(s.c.home, in.Release, in.Volume)
	}
	if !in.Merge {
		if err := s.c.env.RecreateVolume(deployment.WithProgress(ctx, toEvent(emit)), rt, in.Release, vol, recreate); err != nil {
			return res, fmt.Errorf("recreate volume %s: %w: %w", in.Volume, errWrite, err)
		}
	}
	emit(resource.KindVolume, in.Volume, deployment.PhaseCreate, "writing "+in.Name)
	if err := s.importBackup(ctx, in, ms[dst]); err != nil {
		return res, fmt.Errorf("restore volume %s: %w: %w", in.Volume, errWrite, err)
	}
	return res, nil
}

// toEvent turns an emitter back into an event callback, for code that reports through a context.
func toEvent(emit func(kind, name, phase, detail string)) func(deployment.Event) {
	return func(e deployment.Event) { emit(e.Kind, e.Name, e.Phase, e.Detail) }
}

// recreatePlan is what emptying the volume recreates: its Docker volume as applied, and the
// workloads mounting it as applied, with their revisions' files.
func (s *Volumes) recreatePlan(ctx context.Context, rel, volume string) (ports.VolumeRequest, []deployment.Mounter, error) {
	applied, err := s.c.store.ListResources(ctx, rel)
	if err != nil {
		return ports.VolumeRequest{}, nil, err
	}
	var vol ports.VolumeRequest
	var out []deployment.Mounter
	paths := map[int]map[deployment.Key]string{}
	for _, a := range applied {
		r, err := resource.DecodeStored(a.Spec)
		if err != nil && (a.Kind == resource.KindVolume || a.Kind == resource.KindWorkload) {
			return vol, nil, err
		}
		switch spec := r.Spec.(type) {
		case *resource.VolumeSpec:
			if a.Name == volume {
				vol = deployment.VolumeRequest(rel, a.Revision, a.Name, spec)
			}
		case *resource.WorkloadSpec:
			if !slices.ContainsFunc(spec.Mounts, func(m resource.Mount) bool { return m.Volume == volume }) {
				continue
			}
			if paths[a.Revision] == nil {
				if paths[a.Revision], err = s.revisionFiles(ctx, rel, a.Revision); err != nil {
					return vol, nil, err
				}
			}
			out = append(out, deployment.Mounter{Name: a.Name, Container: a.RuntimeName, Digest: a.Digest, Revision: a.Revision, Spec: spec, Paths: paths[a.Revision]})
		}
	}
	return vol, out, nil
}

// revisionFiles materializes a revision's Config and Secret files again (they are its own: the
// containers of that revision mount them) and returns where they are.
func (s *Volumes) revisionFiles(ctx context.Context, rel string, revision int) (map[deployment.Key]string, error) {
	rev, err := s.c.store.GetRevision(ctx, rel, revision)
	if err != nil {
		return nil, fmt.Errorf("revision %d: %w", revision, err)
	}
	rs, err := decodeManifest(rev.Manifest)
	if err != nil {
		return nil, err
	}
	if err := s.c.restoreSecrets(rel, revision, rs); err != nil {
		return nil, err
	}
	return deployment.Materialize(s.c.home, rel, revision, rs, s.c.redact)
}

// keepStopped handles a restore that failed writing the volume, err: the volume may be empty or half
// written, so its workloads stay stopped and the release is paused, which keeps the reconciler from
// starting them, as an interrupted restore that cannot be finished does (resumeRestore).
func (s *Volumes) keepStopped(ctx context.Context, rel release.Release, in restoreInput, res *VolumeResult, err error) error {
	rel.Paused, rel.UpdatedAt = true, s.c.now()
	again := "restore it again"
	if res.RestorePoint != "" || in.Undo {
		again += fmt.Sprintf(" (or 'karga volume undo-restore %s %s')", in.Release, in.Volume)
	}
	err = fmt.Errorf("%w; volume %s may be empty or half written, so %s stay stopped and %s is paused: %s, then 'karga resume %s'",
		err, in.Volume, strings.Join(res.Stopped, ", "), in.Release, again, in.Release)
	return errors.Join(err, s.c.store.SaveRelease(context.WithoutCancel(ctx), rel))
}
