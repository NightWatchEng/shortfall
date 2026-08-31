// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"fmt"
	"strings"
	"time"
)

// maxDurationSeconds caps registry durations at 10 years. SLAs and
// recovery windows beyond that are config errors, and the bound keeps
// every intermediate value in whole-second int64 arithmetic: an
// unbounded day count could wrap positive through int64 nanoseconds
// and turn a 213504-day SLA into a silent 25-minute fence.
const maxDurationSeconds int64 = 10 * 365 * 24 * 3600

// ParseISODuration parses the ISO-8601 duration subset the registry
// speaks: P[nD][T[nH][nM][nS]], strictly positive, at most 10 years.
// Days are exact 24-hour days. Months and years are rejected — their
// length depends on when you ask, and an SLA that means different things
// in February and March is not an SLA.
func ParseISODuration(s string) (time.Duration, error) {
	fail := func() (time.Duration, error) {
		return 0, fmt.Errorf("duration %q is not ISO-8601 (P[nD][T[nH][nM][nS]], positive, <= 10 years)", s)
	}
	rest, ok := strings.CutPrefix(s, "P")
	if !ok || rest == "" {
		return fail()
	}
	datePart, timePart, hasT := strings.Cut(rest, "T")
	if hasT && timePart == "" {
		return fail()
	}
	// All arithmetic in whole seconds, bounded at every step.
	var totalSec int64
	parseUnits := func(part string, units map[byte]int64, order string) error {
		lastIdx := -1
		for part != "" {
			i := 0
			for i < len(part) && part[i] >= '0' && part[i] <= '9' {
				i++
			}
			if i == 0 || i >= len(part) {
				return fmt.Errorf("bad")
			}
			unit := part[i]
			perUnitSec, ok := units[unit]
			if !ok {
				return fmt.Errorf("bad")
			}
			idx := strings.IndexByte(order, unit)
			if idx <= lastIdx {
				return fmt.Errorf("bad")
			}
			lastIdx = idx
			var n int64
			for j := 0; j < i; j++ {
				n = n*10 + int64(part[j]-'0')
				if n > maxDurationSeconds {
					return fmt.Errorf("bad")
				}
			}
			if n > maxDurationSeconds/perUnitSec {
				return fmt.Errorf("bad")
			}
			totalSec += n * perUnitSec
			if totalSec > maxDurationSeconds {
				return fmt.Errorf("bad")
			}
			part = part[i+1:]
		}
		return nil
	}
	if datePart != "" {
		if err := parseUnits(datePart, map[byte]int64{'D': 86400}, "D"); err != nil {
			return fail()
		}
	}
	if hasT {
		if err := parseUnits(timePart, map[byte]int64{'H': 3600, 'M': 60, 'S': 1}, "HMS"); err != nil {
			return fail()
		}
	}
	if totalSec <= 0 {
		return fail()
	}
	return time.Duration(totalSec) * time.Second, nil
}
