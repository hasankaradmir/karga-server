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
		emit(resource.KindWorkload, n, deployment.PhaseDone, "ready")
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

// Restore writes the backup read from r into req.Volume, with the workloads mounting it, and those
// that depend on them, stopped; they run again, ready, before it returns.
func (s *Volumes) Restore(ctx context.Context, req VolumeRestoreRequest, r io.Reader, progress func(deployment.Event)) (res *VolumeResult, err error) {
	res = &VolumeResult{}
	emit := emitter(progress)
	unlock, err := s.c.locks.lock(ctx, req.Release)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ms, err := s.mounters(ctx, req.Release, req.Volume)
	if err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(ctx, req.Release, ms, false)
	if err != nil {
		return nil, err
	}
	dst := -1
	for i, m := range ms {
		if !m.readOnly {
			dst = i
			break
		}
	}
	if dst < 0 {
		return nil, fmt.Errorf("every workload mounts volume %q read-only; nothing can write to it", req.Volume)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("%s is not a karga volume backup: %w", req.Name, err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != backupManifest {
		return nil, fmt.Errorf("%s is not a karga volume backup", req.Name)
	}
	var man BackupManifest
	if err := yaml.NewDecoder(tr).Decode(&man); err != nil {
		return nil, fmt.Errorf("%s: manifest: %w", req.Name, err)
	}
	if (man.Release != req.Release || man.Volume != req.Volume) && !req.Force {
		return nil, fmt.Errorf("%s is a backup of volume %q of release %s; use --from-other-volume to restore it into %q of %s",
			req.Name, man.Volume, man.Release, req.Volume, req.Release)
	}

	rel, err := s.c.store.GetRelease(ctx, req.Release)
	if err != nil {
		return nil, err
	}
	op, err := s.c.begin(ctx, req.Release, "volume-restore", rel.CurrentRevision, false, release.Options{})
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
			err = fmt.Errorf("volume %s of %s restored, but %w", req.Volume, req.Release, err)
		}
	}()
	if res.Stopped, err = stop(ctx, rt, sc, emit); err != nil {
		return res, err
	}
	target := ms[dst]
	emit(resource.KindVolume, req.Volume, deployment.PhaseCreate, "writing "+cmp.Or(req.Name, "the backup"))
	pr, pw := io.Pipe()
	converted := make(chan struct{})
	// The conversion ends before this returns, even if the import stopped reading early.
	defer func() { _ = pr.Close(); <-converted }()
	go func() {
		defer close(converted)
		tw := tar.NewWriter(pw)
		err := retar(tr, tw, backupRoot, path.Base(target.path))
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	if err := rt.ImportPath(ctx, target.runtime, path.Dir(target.path), pr); err != nil {
		_ = pr.CloseWithError(err)
		return res, fmt.Errorf("restore volume %s: %w: %w", req.Volume, errWrite, err)
	}
	return res, nil
}

// errWrite marks a restore that failed writing the files: it did not restore anything to report.
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
