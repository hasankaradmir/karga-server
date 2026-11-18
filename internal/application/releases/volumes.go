package releases

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/resource"

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

// pause stops the running workloads among ms and returns a function starting them again.
func pause(ctx context.Context, rt ports.Runtime, ms []mounter) (func() error, error) {
	var stopped []string
	resume := func() error {
		var errs []error
		for _, n := range stopped {
			errs = append(errs, rt.StartWorkload(context.WithoutCancel(ctx), n))
		}
		return errors.Join(errs...)
	}
	for _, m := range ms {
		st, err := rt.GetWorkload(ctx, m.runtime)
		if err != nil {
			return resume, err
		}
		if st.State != ports.StateRunning {
			continue
		}
		if err := rt.StopWorkload(ctx, m.runtime, 0); err != nil {
			return resume, err
		}
		stopped = append(stopped, m.runtime)
	}
	return resume, nil
}

// BackupVolume writes req.Volume to w.
func (s *Volumes) BackupVolume(ctx context.Context, req VolumeBackupRequest, w io.Writer) (err error) {
	// A live copy only reads: it holds nothing, so a slow client never blocks the release. A copy
	// with --stop stops workloads and holds the release, as an operation, until they run again.
	if req.Stop {
		unlock, err := s.c.locks.lock(ctx, req.Release)
		if err != nil {
			return err
		}
		defer unlock()
	}
	ms, err := s.mounters(ctx, req.Release, req.Volume)
	if err != nil {
		return err
	}
	if req.Stop {
		rel, err := s.c.store.GetRelease(ctx, req.Release)
		if err != nil {
			return err
		}
		op, err := s.c.begin(ctx, req.Release, "volume-backup", rel.CurrentRevision, false, release.Options{})
		if err != nil {
			return err
		}
		defer func() { s.c.finish(ctx, op, err) }()
	} else {
		s.c.log.Info("volume backup", "release", req.Release, "volume", req.Volume, "user", actorOf(ctx))
	}
	rt := s.c.rt
	if req.Stop {
		resume, perr := pause(ctx, rt, ms)
		defer func() { err = errors.Join(err, resume()) }()
		if perr != nil {
			return perr
		}
	}
	src := ms[0]
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

// RestoreVolume writes the backup read from r into req.Volume.
func (s *Volumes) RestoreVolume(ctx context.Context, req VolumeRestoreRequest, r io.Reader) (err error) {
	unlock, err := s.c.locks.lock(ctx, req.Release)
	if err != nil {
		return err
	}
	defer unlock()
	ms, err := s.mounters(ctx, req.Release, req.Volume)
	if err != nil {
		return err
	}
	dst := -1
	for i, m := range ms {
		if !m.readOnly {
			dst = i
			break
		}
	}
	if dst < 0 {
		return fmt.Errorf("every workload mounts volume %q read-only; nothing can write to it", req.Volume)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%s is not a karga volume backup: %w", req.Name, err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != backupManifest {
		return fmt.Errorf("%s is not a karga volume backup", req.Name)
	}
	var man BackupManifest
	if err := yaml.NewDecoder(tr).Decode(&man); err != nil {
		return fmt.Errorf("%s: manifest: %w", req.Name, err)
	}
	if (man.Release != req.Release || man.Volume != req.Volume) && !req.Force {
		return fmt.Errorf("%s is a backup of volume %q of release %s; use --from-other-volume to restore it into %q of %s",
			req.Name, man.Volume, man.Release, req.Volume, req.Release)
	}

	rel, err := s.c.store.GetRelease(ctx, req.Release)
	if err != nil {
		return err
	}
	op, err := s.c.begin(ctx, req.Release, "volume-restore", rel.CurrentRevision, false, release.Options{})
	if err != nil {
		return err
	}
	defer func() { s.c.finish(ctx, op, err) }()
	rt := s.c.rt
	resume, err := pause(ctx, rt, ms)
	defer func() { err = errors.Join(err, resume()) }()
	if err != nil {
		return err
	}
	target := ms[dst]
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
		return fmt.Errorf("restore volume %s: %w", req.Volume, err)
	}
	return nil
}

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
