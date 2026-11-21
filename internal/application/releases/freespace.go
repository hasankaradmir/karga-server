package releases

import "syscall"

// freeSpace is how many bytes an unprivileged user can still write on the filesystem of dir.
func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // Bsize is positive
}
