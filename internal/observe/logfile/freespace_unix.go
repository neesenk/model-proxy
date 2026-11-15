//go:build !windows

package logfile

import "golang.org/x/sys/unix"

// volumeFreeSpace reports the bytes available to unprivileged writes and the
// total capacity of the volume holding dir. Statfs's Bavail excludes
// root-reserved space, matching what the guard may realistically free.
func volumeFreeSpace(dir string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}
