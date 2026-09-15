//go:build linux || darwin || freebsd

package pressure

import "syscall"

// probeDisk reports the bytes available to an unprivileged writer (Bavail, not
// Bfree: the root reserve is not ours to fill) on the filesystem holding dir.
func probeDisk(dir string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, true
}
