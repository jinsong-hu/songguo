package status

import (
	"os"
	"strconv"
	"strings"
)

// The readers below take a file's contents and return what the page needs. They
// are kept apart from the file reads so every platform tests them; on a machine
// without /proc (a dev laptop) the read fails and the reading is simply absent.

// clockTicks is USER_HZ, the unit of /proc/[pid]/stat CPU times. It is 100 on
// every mainstream Linux architecture and not something a container changes.
const clockTicks = 100

// parseLoadavg reads the 1, 5 and 15 minute load averages from /proc/loadavg:
//
//	0.42 0.56 0.65 1/523 12345
func parseLoadavg(s string) (l1, l5, l15 float64, ok bool) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	var err1, err5, err15 error
	l1, err1 = strconv.ParseFloat(f[0], 64)
	l5, err5 = strconv.ParseFloat(f[1], 64)
	l15, err15 = strconv.ParseFloat(f[2], 64)
	return l1, l5, l15, err1 == nil && err5 == nil && err15 == nil
}

// parseCPUTimes sums the aggregate `cpu` line of /proc/stat into busy and total
// ticks. Idle and iowait are the idle share; steal is left out of both, since it
// is time the hypervisor gave to another guest and was never ours to spend.
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
func parseCPUTimes(s string) (busy, total uint64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[0] != "cpu" {
			continue
		}
		v := make([]uint64, 0, 8)
		for _, x := range f[1:9] {
			n, err := strconv.ParseUint(x, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			v = append(v, n)
		}
		for len(v) < 8 {
			v = append(v, 0)
		}
		user, nice, system, idle, iowait, irq, softirq := v[0], v[1], v[2], v[3], v[4], v[5], v[6]
		busy = user + nice + system + irq + softirq
		return busy, busy + idle + iowait, true
	}
	return 0, 0, false
}

// parsePSI reads the `some` line of a /proc/pressure file: the share of the last
// 10 and 60 seconds in which at least one task was stalled on the resource.
//
//	some avg10=2.63 avg60=2.31 avg300=2.14 total=133067887
func parsePSI(s string) (avg10, avg60 float64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "some" {
			continue
		}
		a10, ok10 := cutFloat(f[1], "avg10=")
		a60, ok60 := cutFloat(f[2], "avg60=")
		return a10, a60, ok10 && ok60
	}
	return 0, 0, false
}

// parseNetDev sums received and transmitted bytes over every interface in
// /proc/net/dev except loopback. The file is per network namespace: inside a
// container it is the container's own traffic.
//
//	Inter-|   Receive                            ...|  Transmit
//	 face |bytes    packets errs drop fifo frame ...|bytes    packets ...
//	  eth0: 1234 5 0 0 0 0 0 0 5678 9 0 0 0 0 0 0
func parseNetDev(s string) (rx, tx uint64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "lo" || len(f) < 9 {
			continue
		}
		r, errR := strconv.ParseUint(f[0], 10, 64)
		t, errT := strconv.ParseUint(f[8], 10, 64)
		if errR != nil || errT != nil {
			continue
		}
		rx, tx, ok = rx+r, tx+t, true
	}
	return rx, tx, ok
}

// parseSelfCPU reads utime+stime, in clock ticks, from /proc/self/stat. The
// command name in field 2 is parenthesised and may contain spaces, so fields are
// counted from the last ')'.
func parseSelfCPU(s string) (ticks uint64, ok bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	// After ')' come fields 3.. ; utime and stime are fields 14 and 15.
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0, false
	}
	u, errU := strconv.ParseUint(f[11], 10, 64)
	st, errS := strconv.ParseUint(f[12], 10, 64)
	return u + st, errU == nil && errS == nil
}

// parseStatmRSS reads resident pages from /proc/self/statm (the second field)
// and converts them to bytes.
func parseStatmRSS(s string, pageSize int) (uint64, bool) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(pageSize), err == nil
}

// parseSelfIO reads read_bytes and write_bytes from /proc/self/io: bytes this
// process caused to be fetched from or sent to the storage layer, which is the
// disk load songguo itself puts on the box (page-cache hits are not in it).
func parseSelfIO(s string) (read, write uint64, ok bool) {
	var haveR, haveW bool
	for _, line := range strings.Split(s, "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "read_bytes":
			read, haveR = n, true
		case "write_bytes":
			write, haveW = n, true
		}
	}
	return read, write, haveR && haveW
}

func cutFloat(field, prefix string) (float64, bool) {
	v, found := strings.CutPrefix(field, prefix)
	if !found {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}

func readFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}
