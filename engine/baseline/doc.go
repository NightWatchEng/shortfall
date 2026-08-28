// Package baseline estimates expected stage-entry volume with an interval —
// the counterfactual the unrealized leg measures loss against (M7). v0 is the
// hour-of-week robust median of ADR-0006: explainable to Finance in one
// sentence ("expected volume is the median of the same hour over the last N
// non-holiday weeks; the ± is how much that hour normally varies") and
// deterministic, so two runs over the same data and registry agree in a
// postmortem. A smarter estimator (seasonal decomposition, a Prophet sidecar)
// is a NEW implementation of the Baseline interface, opt-in per flow via the
// registry — never a silent upgrade of the default.
//
// Timezone: v0 assumes a fixed-offset registry timezone (e.g. UTC). Hour-of-week
// bucketing reads each instant in its own location, and the lookback cutoff uses
// calendar arithmetic; across a DST transition those two can disagree by an
// hour, so seasonality for a DST-observing zone is a later refinement, not a v0
// guarantee.
package baseline
