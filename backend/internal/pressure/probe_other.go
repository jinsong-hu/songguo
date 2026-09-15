//go:build !linux

package pressure

// probeMemory has no portable source outside Linux; the memory signal is simply
// absent on a dev machine, and never escalates on its own.
func probeMemory() (avail, total uint64, ok bool) { return 0, 0, false }

// probeIO has no PSI outside Linux; the I/O pressure signal is absent.
func probeIO() (someAvg10 float64, ok bool) { return 0, false }
