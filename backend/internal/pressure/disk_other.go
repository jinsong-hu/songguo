//go:build !(linux || darwin || freebsd)

package pressure

// probeDisk is unavailable on this platform; the disk signal is absent.
func probeDisk(dir string) (free, total uint64, ok bool) { return 0, 0, false }
