//go:build linux

package pressure

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// probeMemory reads the tighter of two budgets: the container's cgroup v2 limit,
// when one is set, and the host's MemAvailable. A container without a limit is
// bounded by the host alone, and a container with one can still starve the host
// if its limit is larger than what the host has left, so both are consulted and
// the one with the smaller free share wins.
func probeMemory() (avail, total uint64, ok bool) {
	hostAvail, hostTotal, hostOK := hostMemory()
	cgAvail, cgTotal, cgOK := cgroupMemory()
	switch {
	case hostOK && cgOK:
		if float64(cgAvail)/float64(cgTotal) < float64(hostAvail)/float64(hostTotal) {
			return cgAvail, cgTotal, true
		}
		return hostAvail, hostTotal, true
	case cgOK:
		return cgAvail, cgTotal, true
	default:
		return hostAvail, hostTotal, hostOK
	}
}

// hostMemory parses MemTotal and MemAvailable from /proc/meminfo. Inside a
// container without lxcfs these are the host's figures, which is exactly the
// budget a box without swap runs out of.
func hostMemory() (avail, total uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var haveAvail, haveTotal bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = kb<<10, true
		case "MemAvailable:":
			avail, haveAvail = kb<<10, true
		}
	}
	return avail, total, haveAvail && haveTotal && total > 0
}

// cgroupMemory reports headroom under a cgroup v2 memory limit. Usage excludes
// inactive file cache, which the kernel reclaims before it OOM-kills, so a
// container full of cold page cache is not mistaken for one out of memory.
func cgroupMemory() (avail, total uint64, ok bool) {
	limit, ok := readUint("/sys/fs/cgroup/memory.max")
	if !ok || limit == 0 {
		return 0, 0, false // "max" (no limit) fails to parse, which is the point
	}
	current, ok := readUint("/sys/fs/cgroup/memory.current")
	if !ok {
		return 0, 0, false
	}
	used := current
	if inactive, ok := memoryStat("/sys/fs/cgroup/memory.stat", "inactive_file"); ok && inactive < used {
		used -= inactive
	}
	if used >= limit {
		return 0, limit, true
	}
	return limit - used, limit, true
}

// probeIO reads host I/O pressure from PSI: the `some avg10` figure, the share
// of the last ten seconds in which at least one task waited on I/O. It is
// deliberately the host's figure, not this cgroup's: the failure being avoided
// is the whole box stalling, and our own writes are what we can take off it. A
// kernel without PSI simply has no reading.
func probeIO() (someAvg10 float64, ok bool) {
	b, err := os.ReadFile("/proc/pressure/io")
	if err != nil {
		return 0, false
	}
	return parsePSISomeAvg10(string(b))
}

func readUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

func memoryStat(path, key string) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, val, found := strings.Cut(sc.Text(), " ")
		if found && name == key {
			n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}
