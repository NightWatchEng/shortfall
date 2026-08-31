// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package checkout

// DefaultCurve is a realistic hour-of-week arrival-rate curve (mean
// arrivals per minute), Monday 00:00 UTC = index 0. Shape: weekday
// business-hours peak (~6/min), evening shoulder, deep night trough
// (~0.4/min), weekends at roughly 60% of weekday volume. The absolute
// numbers only need to be plausible — the engine is judged against the
// ledger this curve generates, not against the real world.
func DefaultCurve() [168]float64 {
	var c [168]float64
	// One weekday's 24-hour shape.
	weekday := [24]float64{
		0.6, 0.4, 0.4, 0.4, 0.5, 0.8, // 00-05: night trough
		1.5, 2.5, 4.0, 5.5, 6.0, 6.0, // 06-11: morning ramp to peak
		5.0, 5.5, 6.0, 5.5, 5.0, 4.0, // 12-17: afternoon plateau
		3.0, 2.5, 2.0, 1.5, 1.0, 0.8, // 18-23: evening decay
	}
	for day := 0; day < 7; day++ {
		scale := 1.0
		if day >= 5 { // Saturday, Sunday
			scale = 0.6
		}
		for h := 0; h < 24; h++ {
			c[day*24+h] = weekday[h] * scale
		}
	}
	return c
}
