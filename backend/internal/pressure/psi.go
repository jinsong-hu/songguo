package pressure

import (
	"strconv"
	"strings"
)

// parsePSISomeAvg10 extracts `avg10` from the `some` line of a PSI file:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=249126552
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=239926891
//
// Kept apart from the Linux-only probe so it is tested on every platform.
func parsePSISomeAvg10(s string) (float64, bool) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		v, found := strings.CutPrefix(fields[1], "avg10=")
		if !found {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	return 0, false
}
