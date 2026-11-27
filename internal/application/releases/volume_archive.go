// The volume backup format and reading and writing it.

package releases

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

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

// checkBackup refuses a file that is not a karga volume backup, or one of another volume without Force.
func (s *Volumes) checkBackup(in restoreInput) error {
	f, err := os.Open(in.From)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Read to the end before the volume is emptied: a file cut short fails its gzip check here.
	if gz, err := gzip.NewReader(f); err == nil {
		_, err = io.Copy(io.Discard, gz)
		if err != nil {
			return fmt.Errorf("%s is cut short or damaged: %w: %w", in.Name, ports.ErrInvalid, err)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
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
