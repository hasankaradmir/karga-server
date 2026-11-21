package releases

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// A volume backup is a gzipped tar: a manifest entry, then the volume's files below "data/".
const (
	backupManifest = "karga-backup.yaml"
	backupRoot     = "data"
)

// BackupManifest describes a volume backup file.
type BackupManifest struct {
	Release   string    `yaml:"release"`
	Volume    string    `yaml:"volume"`
	MountPath string    `yaml:"mountPath"`
	Created   time.Time `yaml:"created"`
}

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
			err = s.c.env.WaitReady(ctx, rt, map[string]*resource.WorkloadSpec{sc.container[n]: sc.spec[n]}, 0)
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
	op, err := s.c.begin(ctx, req.Release, "volume-backup", rel.CurrentRevision, false, release.Options{})
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
	err = errors.Join(err, s.start(ctx, rt, sc, res.Stopped, emit))
	if len(res.Stopped) > 0 {
		res.Downtime = s.c.now().Sub(began)
	}
	return err
}

// write copies the volume, through the workload src that mounts it, to w as a backup archive.
func (s *Volumes) write(ctx context.Context, req VolumeBackupRequest, src mounter, w io.Writer) error {
	rt := s.c.rt
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	man, _ := yaml.Marshal(BackupManifest{Release: req.Release, Volume: req.Volume, MountPath: src.path, Created: s.c.now().UTC()})
	if err := tw.WriteHeader(&tar.Header{Name: backupManifest, Mode: 0o600, Size: int64(len(man)), ModTime: s.c.now()}); err != nil {
		return err
	}
	if _, err := tw.Write(man); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	exported := make(chan struct{})
	go func() { defer close(exported); pw.CloseWithError(rt.ExportPath(ctx, src.runtime, src.path, pw)) }()
	// The export ends before this returns: closing the reader makes a write it is blocked on fail.
	defer func() { _ = pr.Close(); <-exported }()
	if err := retar(tar.NewReader(pr), tw, path.Base(src.path), backupRoot); err != nil {
		_ = pr.CloseWithError(err)
		return fmt.Errorf("copy volume %s: %w", req.Volume, err)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
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

// restorePointPath is where a restore keeps a volume's previous files.
func restorePointPath(home, rel, vol string) string {
	return filepath.Join(home, "restore-points", rel, vol+".tar.gz")
}

// restorePointStep marks, in a restore's steps, that its restore point was kept.
const restorePointStep = "restore-point"

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
	op, err := s.c.begin(ctx, in.Release, "volume-restore", rel.CurrentRevision, resumed, release.Options{Restore: &release.RestoreOptions{
		Volume: in.Volume, From: in.From, Name: in.Name, Merge: in.Merge, NoRestorePoint: in.NoRestorePoint, Undo: in.Undo}})
	if err != nil {
		return nil, err
	}
	defer func() { op.Downtime = res.Downtime; s.c.finish(ctx, op, err) }()
	rt := s.c.rt
	began := s.c.now()
	defer func() {
		err = errors.Join(err, s.start(ctx, rt, sc, res.Stopped, emit))
		if len(res.Stopped) > 0 {
			res.Downtime = s.c.now().Sub(began)
		}
		if errors.Is(err, ErrNotReady) && !errors.Is(err, errWrite) {
			err = fmt.Errorf("volume %s of %s restored, but %w", in.Volume, in.Release, err)
		}
	}()
	if res.Stopped, err = stop(ctx, rt, sc, emit); err != nil {
		return res, err
	}
	if point {
		emit(resource.KindVolume, in.Volume, deployment.PhaseCreate, "keeping its files as a restore point")
		if res.RestorePoint, err = s.saveRestorePoint(ctx, in, ms[0]); err != nil {
			return res, err
		}
		op.Steps = append(op.Steps, release.Step{Kind: resource.KindVolume, Name: in.Volume, Action: restorePointStep, Status: deployment.StepDone})
		if err := s.c.store.SaveOperationSteps(ctx, op.ID, op.Steps); err != nil {
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

// checkBackup refuses a file that is not a karga volume backup, or one of another volume without Force.
func (s *Volumes) checkBackup(in restoreInput) error {
	f, err := os.Open(in.From)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	man, _, err := openBackup(f, in.Name)
	if err != nil {
		return err
	}
	if (man.Release != in.Release || man.Volume != in.Volume) && !in.Force {
		return fmt.Errorf("%s is a backup of volume %q of release %s; use --from-other-volume to restore it into %q of %s: %w",
			in.Name, man.Volume, man.Release, in.Volume, in.Release, ports.ErrInvalid)
	}
	return nil
}

// openBackup reads a backup's manifest and returns the reader of the files after it.
func openBackup(r io.Reader, name string) (BackupManifest, *tar.Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return BackupManifest{}, nil, fmt.Errorf("%s is not a karga volume backup: %w: %w", name, ports.ErrInvalid, err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != backupManifest {
		return BackupManifest{}, nil, fmt.Errorf("%s is not a karga volume backup: %w", name, ports.ErrInvalid)
	}
	var man BackupManifest
	if err := yaml.NewDecoder(tr).Decode(&man); err != nil {
		return BackupManifest{}, nil, fmt.Errorf("%s: manifest: %w: %w", name, ports.ErrInvalid, err)
	}
	return man, tr, nil
}

// importBackup writes the backup's files into the volume, through dst (stopped or created). It
// writes at the mount itself, not its parent: that works with a read-only root filesystem too.
func (s *Volumes) importBackup(ctx context.Context, in restoreInput, dst mounter) error {
	f, err := os.Open(in.From)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, tr, err := openBackup(f, in.Name)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	converted := make(chan struct{})
	// The conversion ends before this returns, even if the import stopped reading early.
	defer func() { _ = pr.Close(); <-converted }()
	go func() {
		defer close(converted)
		tw := tar.NewWriter(pw)
		err := retar(tr, tw, backupRoot, ".")
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	if err := s.c.rt.ImportPath(ctx, dst.runtime, dst.path, pr); err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	return nil
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

// roomForRestorePoint refuses a restore whose restore point (about as large as the backup) would not
// fit under KARGA_HOME.
func (s *Volumes) roomForRestorePoint(from string) error {
	st, err := os.Stat(from)
	if err != nil {
		return err
	}
	need := uint64(st.Size()) + uint64(st.Size())/10 //nolint:gosec // a file size is not negative
	free, err := s.c.freeSpace(s.c.home)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("not enough space under %s for a restore point (%d MB free, about %d MB needed): free some, or restore with --no-restore-point: %w",
			s.c.home, free>>20, need>>20, ports.ErrInvalid)
	}
	return nil
}

// saveRestorePoint copies the volume, through src, to its restore point, replacing the last one only
// once the copy is complete.
func (s *Volumes) saveRestorePoint(ctx context.Context, in restoreInput, src mounter) (string, error) {
	dst := restorePointPath(s.c.home, in.Release, in.Volume)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	err = s.write(ctx, VolumeBackupRequest{Release: in.Release, Volume: in.Volume}, src, f)
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		err = os.Rename(dst+".tmp", dst)
	}
	if err != nil {
		_ = os.Remove(dst + ".tmp")
		if errors.Is(err, syscall.ENOSPC) {
			return "", fmt.Errorf("not enough space under %s for a restore point: free some, or restore with --no-restore-point: %w", s.c.home, err)
		}
		return "", fmt.Errorf("restore point: %w", err)
	}
	return dst, nil
}

// errWrite marks a restore that failed changing the volume: it did not restore anything to report.
var errWrite = errors.New("writing the backup failed")

// retar copies tar entries below from/ to below to/ (the entry from itself becomes to).
func retar(tr *tar.Reader, tw *tar.Writer, from, to string) error {
	rename := func(name string) (string, bool) {
		name = strings.TrimPrefix(name, "./")
		if name == from || name == from+"/" {
			return to + "/", true
		}
		rest, ok := strings.CutPrefix(name, from+"/")
		return to + "/" + rest, ok
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok := rename(h.Name)
		if !ok {
			continue
		}
		h.Name = name
		if h.Typeflag == tar.TypeLink {
			if h.Linkname, ok = rename(h.Linkname); !ok {
				continue
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}
