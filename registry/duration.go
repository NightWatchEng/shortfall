package registry

import (
	"fmt"
	"strings"
	"time"
)

// ParseISODuration parses the ISO-8601 duration subset the registry
// speaks: P[nD][T[nH][nM][nS]], strictly positive. Days are exact
// 24-hour days. Months and years are rejected — their length depends on
// when you ask, and an SLA that means different things in February and
// March is not an SLA.
func ParseISODuration(s string) (d time.Duration, err error) {
	fail := func() (time.Duration, error) {
		return 0, fmt.Errorf("duration %q is not ISO-8601 (P[nD][T[nH][nM][nS]], positive)", s)
	}
	rest, ok := strings.CutPrefix(s, "P")
	if !ok || rest == "" {
		return fail()
	}
	datePart, timePart, hasT := strings.Cut(rest, "T")
	if hasT && timePart == "" {
		return fail()
	}
	parseUnits := func(part string, units map[byte]time.Duration, order string) (time.Duration, error) {
		var total time.Duration
		lastIdx := -1
		for part != "" {
			i := 0
			for i < len(part) && part[i] >= '0' && part[i] <= '9' {
				i++
			}
			if i == 0 || i >= len(part) {
				return 0, fmt.Errorf("bad")
			}
			unit := part[i]
			mult, ok := units[unit]
			if !ok {
				return 0, fmt.Errorf("bad")
			}
			idx := strings.IndexByte(order, unit)
			if idx <= lastIdx {
				return 0, fmt.Errorf("bad")
			}
			lastIdx = idx
			var n int64
			for j := 0; j < i; j++ {
				n = n*10 + int64(part[j]-'0')
				if n > 1<<31 {
					return 0, fmt.Errorf("bad")
				}
			}
			total += time.Duration(n) * mult
			part = part[i+1:]
		}
		return total, nil
	}
	var total time.Duration
	if datePart != "" {
		dd, derr := parseUnits(datePart, map[byte]time.Duration{'D': 24 * time.Hour}, "D")
		if derr != nil {
			return fail()
		}
		total += dd
	}
	if hasT {
		td, terr := parseUnits(timePart, map[byte]time.Duration{
			'H': time.Hour, 'M': time.Minute, 'S': time.Second,
		}, "HMS")
		if terr != nil {
			return fail()
		}
		total += td
	}
	if total <= 0 {
		return fail()
	}
	return total, nil
}
