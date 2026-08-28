package engine

import (
	"github.com/NightWatchEng/shortfall/registry"
)

// SuggestSeverity maps the incident's dollars-per-minute-at-risk to a suggested
// severity from the registry's threshold ladder (ADR-0013). "At risk" is the
// realized loss plus the deferred (in-flight) value — money the incident has
// already lost or put at stake — divided by the window's minutes.
//
// The rate is evaluated PER CURRENCY (ADR-0001: no cross-currency sum) and the
// suggestion is the MOST-SEVERE level any currency triggers — a $2M/hour flow
// pages even if a co-incident low-value flow would not. Returns "" when there
// is no ladder, no window, or nothing clears the lowest threshold — never a
// fabricated severity.
func SuggestSeverity(reg *registry.Registry, r Report) string {
	if reg == nil || len(reg.Severity) == 0 {
		return ""
	}
	minutes := r.Request.Window.To.Sub(r.Request.Window.From).Minutes()
	if minutes <= 0 {
		return ""
	}

	// Per-currency at-risk = realized loss + deferred in-flight value.
	atRisk := map[string]int64{}
	for cur, v := range r.Realized.ByCurrency {
		atRisk[cur] += v
	}
	for cur, v := range r.Deferred.ByCurrency {
		atRisk[cur] += v
	}

	// Thresholds are ordered most-severe first; the best (lowest) index any
	// currency reaches wins.
	best := len(reg.Severity) // sentinel: nothing triggered
	for _, v := range atRisk {
		rate := float64(v) / minutes
		for i, th := range reg.Severity {
			if rate >= float64(th.MinPerMinuteMinor) {
				if i < best {
					best = i
				}
				break // most-severe threshold this currency clears
			}
		}
	}
	if best == len(reg.Severity) {
		return ""
	}
	return reg.Severity[best].Sev
}
