package baseline

import (
	"fmt"
	"sort"
	"time"
)

// madToStdDev is the normal-consistency factor that scales a median absolute
// deviation to a standard-deviation estimate; intervalSigma widens it to an
// ≈95% band under normality. Both are part of the ADR-0006 contract — two
// conforming implementations must produce the same range — so they are
// constants here, not tuning knobs.
const (
	madToStdDev   = 1.4826
	intervalSigma = 2.0
	hoursPerWeek  = 168
)

// Sample is one historical stage-entry count in an hourly bucket. Count is a
// float because it comes from a time-series backend (events remain the exact
// source of truth elsewhere); baseline math is inherently statistical.
type Sample struct {
	At    time.Time
	Count float64
}

// Expectation is the expected stage-entry count for one target hour with its
// interval (ADR-0006). Lower is clamped at 0 — a negative expected count is
// meaningless. N is how many same-hour-of-week samples the estimate rests on,
// so a caller can flag a thin or empty basis rather than trusting a baseline
// built from too little history.
type Expectation struct {
	At       time.Time
	Expected float64
	Lower    float64
	Upper    float64
	N        int
}

// Config parameters an estimate. The ADR-0006 statistical constants are fixed
// in the implementation, not here. Holiday reports whether an instant falls in
// a holiday whose samples must be excluded; nil means no holidays.
type Config struct {
	LookbackWeeks int
	Holiday       func(time.Time) bool
}

// Baseline estimates expected stage-entry volume with an interval. v0 is
// HourOfWeek (ADR-0006). Additional implementations are opt-in per flow.
type Baseline interface {
	// Expected returns one Expectation per target instant, in target order,
	// computed from the subset of history within Config.LookbackWeeks before the
	// earliest target instant. Callers may pass a wider history (e.g. a whole
	// query result); the implementation enforces the lookback bound and excludes
	// samples at or after the target window. An empty target yields nil.
	Expected(history []Sample, target []time.Time, cfg Config) ([]Expectation, error)
}

// HourOfWeek is the ADR-0006 v0 baseline: for each target hour, the robust
// median (and MAD interval) of the same hour-of-week across the lookback,
// holidays excluded.
type HourOfWeek struct{}

// Expected implements Baseline. It bounds history to the LookbackWeeks window
// ending at the earliest target instant, buckets the (non-holiday) counts into
// the 168 hours of the week (Monday 00:00 = 0, matching the harness curve), and
// for each target instant returns the median of its bucket ± the MAD band.
func (HourOfWeek) Expected(history []Sample, target []time.Time, cfg Config) ([]Expectation, error) {
	if cfg.LookbackWeeks < 1 {
		return nil, fmt.Errorf("baseline: lookback_weeks %d must be >= 1", cfg.LookbackWeeks)
	}
	if len(target) == 0 {
		return nil, nil
	}

	// The lookback is measured back from the earliest target instant: the
	// baseline is the last N weeks before the window, never the window itself.
	// Enforcing it here means a loosely pre-bounded history cannot silently
	// widen the basis.
	earliest := target[0]
	for _, t := range target[1:] {
		if t.Before(earliest) {
			earliest = t
		}
	}
	cutoff := earliest.AddDate(0, 0, -7*cfg.LookbackWeeks)

	// Bucket the in-window, non-holiday history counts by hour-of-week.
	buckets := make([][]float64, hoursPerWeek)
	for _, s := range history {
		if s.At.Before(cutoff) || !s.At.Before(earliest) {
			continue // outside the lookback, or during/after the target window
		}
		if cfg.Holiday != nil && cfg.Holiday(s.At) {
			continue
		}
		h := hourOfWeek(s.At)
		buckets[h] = append(buckets[h], s.Count)
	}

	out := make([]Expectation, 0, len(target))
	for _, t := range target {
		b := buckets[hourOfWeek(t)]
		e := Expectation{At: t, N: len(b)}
		if len(b) > 0 {
			med := median(b)
			band := intervalSigma * madToStdDev * mad(b, med)
			e.Expected = med
			e.Lower = med - band
			if e.Lower < 0 {
				e.Lower = 0
			}
			e.Upper = med + band
		}
		out = append(out, e)
	}
	return out, nil
}

// hourOfWeek maps an instant to 0..167 with Monday 00:00 = 0 (the harness
// curve's convention). Go's Weekday is Sunday=0..Saturday=6; (wd+6)%7 rotates
// Monday to 0. The instant is read in its own location, so callers pin the
// timezone by constructing samples/targets in the zone the registry means.
func hourOfWeek(t time.Time) int {
	dow := (int(t.Weekday()) + 6) % 7 // Monday=0 … Sunday=6
	return dow*24 + t.Hour()
}

// median returns the robust median of xs (unsorted input is copied, not
// mutated). Empty input is a caller bug guarded before this point.
func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// mad is the median absolute deviation of xs about med.
func mad(xs []float64, med float64) float64 {
	dev := make([]float64, len(xs))
	for i, x := range xs {
		d := x - med
		if d < 0 {
			d = -d
		}
		dev[i] = d
	}
	return median(dev)
}
