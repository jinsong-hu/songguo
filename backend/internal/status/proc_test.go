package status

import (
	"os"
	"testing"
)

// The fixtures follow the kernel's layout; the PSI, net/dev and io ones are
// trimmed from the production host (xyz-bj-1).

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, ok := parseLoadavg("0.42 0.56 0.65 1/523 12345\n")
	if !ok || l1 != 0.42 || l5 != 0.56 || l15 != 0.65 {
		t.Errorf("got %v %v %v %v", l1, l5, l15, ok)
	}
	if _, _, _, ok := parseLoadavg("garbage"); ok {
		t.Error("garbage parsed")
	}
}

func TestParseCPUTimes(t *testing.T) {
	const stat = "cpu  100 5 50 800 40 3 2 7 0 0\ncpu0 50 2 25 400 20 1 1 3 0 0\nintr 1 2 3\n"
	busy, total, ok := parseCPUTimes(stat)
	// busy = user+nice+system+irq+softirq; total adds idle+iowait; steal is neither.
	if !ok || busy != 160 || total != 1000 {
		t.Errorf("busy %d total %d ok %v, want 160 1000 true", busy, total, ok)
	}
	if _, _, ok := parseCPUTimes("intr 1 2 3\n"); ok {
		t.Error("parsed without a cpu line")
	}
}

func TestParsePSI(t *testing.T) {
	const io = "some avg10=2.63 avg60=2.31 avg300=2.14 total=133067887\nfull avg10=2.59 avg60=2.20 avg300=2.01 total=125165867\n"
	a10, a60, ok := parsePSI(io)
	if !ok || a10 != 2.63 || a60 != 2.31 {
		t.Errorf("got %v %v %v", a10, a60, ok)
	}
	if _, _, ok := parsePSI("full avg10=1.00 avg60=1.00 avg300=1.00 total=1\n"); ok {
		t.Error("parsed a file with no some line")
	}
}

func TestParseNetDev(t *testing.T) {
	const dev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999    100    0    0    0     0          0         0  9999999     100    0    0    0     0       0          0
  eth0: 1000       10     0    0    0     0          0         0  2000       20     0    0    0     0       0          0
  eth1: 30         1      0    0    0     0          0         0  40         1      0    0    0     0       0          0
`
	rx, tx, ok := parseNetDev(dev)
	if !ok || rx != 1030 || tx != 2040 {
		t.Errorf("rx %d tx %d ok %v, want 1030 2040 true (loopback excluded)", rx, tx, ok)
	}
	if _, _, ok := parseNetDev("Inter-| Receive\n face |bytes\n    lo: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n"); ok {
		t.Error("loopback alone should be no reading")
	}
}

func TestParseSelfCPU(t *testing.T) {
	// A command name with a space and a ')' in it must not shift the fields.
	const stat = "1234 (song guo)) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 75 0 0 20 0 15 0 1000 700000000 180000 0\n"
	ticks, ok := parseSelfCPU(stat)
	if !ok || ticks != 325 {
		t.Errorf("ticks %d ok %v, want 325 true", ticks, ok)
	}
}

func TestParseStatmRSS(t *testing.T) {
	rss, ok := parseStatmRSS("250000 180000 4000 1000 0 90000 0\n", 4096)
	if !ok || rss != 180000*4096 {
		t.Errorf("rss %d ok %v", rss, ok)
	}
}

func TestParseSelfIO(t *testing.T) {
	const io = "rchar: 4831311454\nwchar: 801286668\nsyscr: 1174093\nsyscw: 248226\nread_bytes: 263577600\nwrite_bytes: 630071296\ncancelled_write_bytes: 0\n"
	r, w, ok := parseSelfIO(io)
	if !ok || r != 263577600 || w != 630071296 {
		t.Errorf("read %d write %d ok %v", r, w, ok)
	}
}

// On a machine without /proc every probe is absent rather than zero, which is
// what lets the page say "—" instead of "0% CPU".
func TestReadFileMissing(t *testing.T) {
	if _, err := os.Stat("/proc/definitely-not-here"); err == nil {
		t.Skip("unexpected file")
	}
	if _, ok := readFile("/proc/definitely-not-here"); ok {
		t.Error("missing file read as ok")
	}
}
