// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package baseline

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestMedianAndMAD(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		med  float64
		mad  float64
	}{
		{"odd", []float64{3, 1, 2}, 2, 1},                        // sorted 1,2,3; devs 1,0,1 -> mad 1
		{"even", []float64{1, 2, 3, 4}, 2.5, 1},                  // med 2.5; devs 1.5,0.5,0.5,1.5 -> mad 1
		{"single", []float64{7}, 7, 0},                           // no spread
		{"constant", []float64{5, 5, 5}, 5, 0},                   // zero MAD
		{"with outlier", []float64{10, 10, 10, 10, 1000}, 10, 0}, // median robust to the outlier
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := median(c.xs); got != c.med {
				t.Fatalf("median = %v, want %v", got, c.med)
			}
			if got := mad(c.xs, c.med); got != c.mad {
				t.Fatalf("mad = %v, want %v", got, c.mad)
			}
		})
	}
}

func TestHourOfWeekMondayZero(t *testing.T) {
	// 2024-01-01 was a Monday. Monday 00:00 -> 0; Monday 13:00 -> 13;
	// Tuesday 00:00 -> 24; Sunday 23:00 -> 167.
	mon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		t    time.Time
		want int
	}{
		{mon, 0},
		{mon.Add(13 * time.Hour), 13},
		{mon.Add(24 * time.Hour), 24},
		{mon.Add(167 * time.Hour), 167}, // Sunday 23:00
	}
	for _, c := range cases {
		if got := hourOfWeek(c.t); got != c.want {
			t.Fatalf("hourOfWeek(%v) = %d, want %d", c.t, got, c.want)
		}
	}
}

func TestExpectedRejectsBadLookback(t *testing.T) {
	if _, err := (HourOfWeek{}).Expected(nil, nil, Config{LookbackWeeks: 0}); err == nil {
		t.Fatal("lookback < 1 must error")
	}
}

const week = 7 * 24 * time.Hour

func TestExpectedIntervalAndEmptyBucket(t *testing.T) {
	histMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // Monday, week 0
	target := histMon.Add(4 * week)                        // a later Monday 00:00 (the incident hour)
	// Two prior weeks of the Monday-00:00 bucket: counts 100 and 108.
	hist := []Sample{
		{At: histMon, Count: 100},
		{At: histMon.Add(week), Count: 108},
	}
	exp, err := (HourOfWeek{}).Expected(hist, []time.Time{target, target.Add(time.Hour)}, Config{LookbackWeeks: 8})
	if err != nil {
		t.Fatal(err)
	}
	// Monday 00:00 bucket: median 104, MAD median(|100-104|,|108-104|)=4,
	// band = 2*1.4826*4 = 11.8608 -> [92.14, 115.86], N=2.
	got := exp[0]
	if got.N != 2 || got.Expected != 104 {
		t.Fatalf("expected = %+v, want N2 median 104", got)
	}
	wantBand := 2 * 1.4826 * 4
	if math.Abs(got.Upper-(104+wantBand)) > 1e-9 || math.Abs(got.Lower-(104-wantBand)) > 1e-9 {
		t.Fatalf("interval = [%v,%v], want [%v,%v]", got.Lower, got.Upper, 104-wantBand, 104+wantBand)
	}
	// Monday 01:00 has no history -> empty bucket -> zero-value estimate, N=0.
	if exp[1].N != 0 || exp[1].Expected != 0 || exp[1].Upper != 0 {
		t.Fatalf("empty bucket = %+v, want N0 zeros", exp[1])
	}
}

func TestExpectedLowerClampedAtZero(t *testing.T) {
	histMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	target := histMon.Add(4 * week)
	// Small median with large spread would push the lower bound negative.
	hist := []Sample{
		{At: histMon, Count: 2},
		{At: histMon.Add(week), Count: 20},
		{At: histMon.Add(2 * week), Count: 2},
	}
	exp, err := (HourOfWeek{}).Expected(hist, []time.Time{target}, Config{LookbackWeeks: 8})
	if err != nil {
		t.Fatal(err)
	}
	if exp[0].Lower < 0 {
		t.Fatalf("lower = %v, must be clamped at 0", exp[0].Lower)
	}
}

func TestHolidayExclusion(t *testing.T) {
	histMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // week 0 Monday — a holiday
	target := histMon.Add(5 * week)
	holiday := func(ts time.Time) bool { return ts.Year() == 2024 && ts.YearDay() == 1 }
	// Week 0 (holiday) has an absurd spike; weeks 1-2 are the true level 100.
	hist := []Sample{
		{At: histMon, Count: 99999},             // holiday -> excluded
		{At: histMon.Add(week), Count: 100},     // week 1
		{At: histMon.Add(2 * week), Count: 100}, // week 2
	}
	exp, err := (HourOfWeek{}).Expected(hist, []time.Time{target}, Config{LookbackWeeks: 8, Holiday: holiday})
	if err != nil {
		t.Fatal(err)
	}
	// The holiday spike must not enter the bucket: median 100, N 2.
	if exp[0].N != 2 || exp[0].Expected != 100 {
		t.Fatalf("holiday sample leaked into the estimate: %+v", exp[0])
	}
}

func TestLookbackBoundsHistory(t *testing.T) {
	// History older than LookbackWeeks before the target must be dropped, and
	// samples at/after the target window excluded — the baseline is the last N
	// weeks before the incident, never a wider basis a loose caller passed.
	target := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC) // a Monday
	hist := []Sample{
		{At: target.Add(-10 * week), Count: 1},  // older than the 8-week lookback -> dropped
		{At: target.Add(-2 * week), Count: 100}, // in window
		{At: target.Add(-1 * week), Count: 100}, // in window
		{At: target, Count: 5},                  // at the target instant -> excluded
		{At: target.Add(week), Count: 5},        // after the window -> excluded
	}
	exp, err := (HourOfWeek{}).Expected(hist, []time.Time{target}, Config{LookbackWeeks: 8})
	if err != nil {
		t.Fatal(err)
	}
	if exp[0].N != 2 || exp[0].Expected != 100 {
		t.Fatalf("lookback bounding wrong: %+v, want N2 median 100 (only the two in-window samples)", exp[0])
	}
}

func TestConstantBucketYieldsZeroWidthInterval(t *testing.T) {
	// A genuinely constant hour has zero normal variation: MAD 0 -> a zero-width
	// interval. Documented behavior; the unrealized leg decides how to treat a
	// zero-width band.
	histMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	target := histMon.Add(4 * week)
	hist := []Sample{
		{At: histMon, Count: 50},
		{At: histMon.Add(week), Count: 50},
		{At: histMon.Add(2 * week), Count: 50},
	}
	exp, err := (HourOfWeek{}).Expected(hist, []time.Time{target}, Config{LookbackWeeks: 8})
	if err != nil {
		t.Fatal(err)
	}
	if exp[0].Expected != 50 || exp[0].Lower != 50 || exp[0].Upper != 50 {
		t.Fatalf("constant bucket = %+v, want a zero-width interval at 50", exp[0])
	}
}

// knownCurve is a deterministic 168-hour stage-entry curve (a daily double-peak
// shape scaled by weekday) used as ground truth for the accuracy bar.
func knownCurve() [hoursPerWeek]float64 {
	var c [hoursPerWeek]float64
	for i := 0; i < hoursPerWeek; i++ {
		dow := i / 24
		hour := i % 24
		daily := 200.0 + 150*math.Sin(float64(hour)/24*2*math.Pi) + 100*math.Cos(float64(hour)/24*4*math.Pi)
		weekdayScale := 1.0
		if dow >= 5 { // weekend lull
			weekdayScale = 0.6
		}
		c[i] = 400 + daily*weekdayScale
	}
	return c
}

func TestAccuracyUnderFivePercentOnKnownCurve(t *testing.T) {
	// ADR-0006 accuracy bar: median absolute percentage error < 5% of hourly
	// expected vs the true curve, over a non-holiday evaluation window. Eight
	// weeks of hourly samples are generated from the curve plus symmetric noise;
	// the robust median must recover the curve within 5%.
	curve := knownCurve()
	rng := rand.New(rand.NewSource(20260828))
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // a Monday
	const weeks = 8
	var hist []Sample
	for w := 0; w < weeks; w++ {
		for h := 0; h < hoursPerWeek; h++ {
			at := base.Add(time.Duration(w*hoursPerWeek+h) * time.Hour)
			noise := (rng.Float64()*2 - 1) * 0.10 // ±10% symmetric
			hist = append(hist, Sample{At: at, Count: curve[h] * (1 + noise)})
		}
	}
	// Evaluate over a 4-week window (ADR-0006's evaluation window), all hours
	// estimated from the 8 prior weeks.
	const evalWeeks = 4
	var target []time.Time
	for h := 0; h < evalWeeks*hoursPerWeek; h++ {
		target = append(target, base.Add(time.Duration(weeks*hoursPerWeek+h)*time.Hour))
	}
	exp, err := (HourOfWeek{}).Expected(hist, target, Config{LookbackWeeks: weeks})
	if err != nil {
		t.Fatal(err)
	}

	var apes []float64
	var band, contained int
	for i, e := range exp {
		truth := curve[hourOfWeek(target[i])]
		apes = append(apes, math.Abs(e.Expected-truth)/truth)
		if e.Upper > e.Lower {
			band++
		}
		if truth >= e.Lower && truth <= e.Upper {
			contained++
		}
	}
	// ADR-0006 accuracy bar: median absolute percentage error < 5%.
	if mape := median(apes); mape >= 0.05 {
		t.Fatalf("median absolute percentage error = %.3f, want < 0.05", mape)
	}
	// Every hour is reported as a non-degenerate range (the leg is a range or
	// nothing), and the band — a robust spread of the samples, not a CI on the
	// mean — contains the truth for the large majority of hours.
	if band != len(exp) {
		t.Fatalf("%d/%d hours had a degenerate (zero-width) interval", len(exp)-band, len(exp))
	}
	if contained < len(exp)*8/10 {
		t.Fatalf("interval contained truth for only %d/%d hours, want >= 80%%", contained, len(exp))
	}
}

func BenchmarkExpected(b *testing.B) {
	curve := knownCurve()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const weeks = 8
	var hist []Sample
	for w := 0; w < weeks; w++ {
		for h := 0; h < hoursPerWeek; h++ {
			at := base.Add(time.Duration(w*hoursPerWeek+h) * time.Hour)
			hist = append(hist, Sample{At: at, Count: curve[h]})
		}
	}
	var target []time.Time
	for h := 0; h < hoursPerWeek; h++ {
		target = append(target, base.Add(time.Duration(weeks*hoursPerWeek+h)*time.Hour))
	}
	cfg := Config{LookbackWeeks: weeks}
	bl := HourOfWeek{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := bl.Expected(hist, target, cfg); err != nil {
			b.Fatal(err)
		}
	}
}
