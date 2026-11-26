// Restore points: the files a restore replaced, kept so that an undo-restore can put them back.

package releases

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// restorePointsDir holds the restore points of release rel's volumes.
func restorePointsDir(home, rel string) string {
	return filepath.Join(home, "restore-points", rel)
}

// restorePointPath is where a restore keeps a volume's previous files.
func restorePointPath(home, rel, vol string) string {
	return filepath.Join(restorePointsDir(home, rel), vol+".tar.gz")
}

// restorePointStep marks, in a restore's steps, that its restore point was kept.
const restorePointStep = "restore-point"

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
