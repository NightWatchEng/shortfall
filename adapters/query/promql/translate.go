package promql

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

// gaugeFamilies read as a level (last sample) rather than a delta increase —
// must match the memq reference and the emitter's families.
var gaugeFamilies = map[string]bool{"biz_inflight_value": true, "biz_inflight_count": true}

// promExpr is a translated PromQL instant query. This adapter issues only
// instant queries (evaluated at `at`), because the exact translations below
// use the @ modifier to pin evaluation times rather than a stepped range —
// see translate for why stepped queries are refused.
type promExpr struct {
	expr string
	at   time.Time
}

// translate turns a frozen query.Query into PromQL whose numbers match the
// in-memory reference (query/memq) rather than PromQL's rate-oriented
// helpers:
//
//   - Counter families: the exact increase over the window is the cumulative
//     counter's difference between the two ends,
//     `<end> - (<start> or (<end> * 0))`, where each end reads the last
//     cumulative sample at that boundary via last_over_time (see below for why
//     last_over_time and not a bare `@` instant, and why the `or (<end>*0)`
//     fill). This avoids increase(), which extrapolates to the range
//     boundaries and drops the first in-range sample — producing an
//     estimate, not the exact minor-unit sum memq computes. It is exact for a
//     monotonic counter (our value/count families never decrease) up to
//     sample-boundary alignment; a series present at only one end is handled
//     (the `or` fill starts it from 0, as memq does), while counter resets
//     remain an unhandled edge case.
//   - The gauge family: `sum by(g)(last_over_time(m[window] @ To))` — the last
//     level at To, carried forward across the window, matching memq's
//     carry-forward (a plain instant would use Prometheus's ~5m staleness
//     window instead).
//
// Both boundaries are evaluated one millisecond inside the window (To-1ms,
// From-1ms) to realize the half-open [From, To) window; see the body.
//
// Only Step==0 (one bucket over the whole range) is supported: Prometheus
// range-step increase() buckets look backward from each step boundary while
// memq buckets forward from each start, so a stepped translation would be
// one step misaligned. The engine issues only Step==0 metric queries.
func translate(q query.Query) (promExpr, error) {
	if q.Agg == query.AggCount {
		return promExpr{}, fmt.Errorf("promql: AggCount is not supported; the engine reads counts as AggSum over biz_txn_total")
	}
	if q.Step > 0 {
		return promExpr{}, fmt.Errorf("promql: stepped (Step>0) queries are not supported yet (bucket alignment vs the memq reference); the engine uses Step==0 window queries — see workspace-0ka")
	}
	matchers := labelMatchers(q.Filters)
	by := groupBy(q.GroupBy)

	// The engine window is half-open [From, To) — memq counts samples with
	// From <= At < To. A bare `@ To` is inclusive (<= To) and `@ From` includes
	// From, which would give (From, To]. Evaluate one millisecond before each
	// boundary so `<= To-1ms` is `< To` and `<= From-1ms` is `< From`, matching
	// memq exactly (Prometheus timestamps are millisecond-resolution and the
	// data is coarser).
	evalTo := q.Range.To.Add(-time.Millisecond)
	evalFrom := q.Range.From.Add(-time.Millisecond)
	at := evalTo

	if gaugeFamilies[q.Metric] {
		rng := q.Range.To.Sub(q.Range.From)
		return promExpr{
			expr: fmt.Sprintf("sum %s(last_over_time(%s%s[%s] @ %s))", by, q.Metric, matchers, promDuration(rng), promTime(evalTo)),
			at:   at,
		}, nil
	}

	// Counter: exact non-extrapolating cumulative difference across the window,
	// end minus start. Each boundary reads the last cumulative sample in the
	// window via last_over_time, not a bare `@` instant: `@` uses a 5-minute
	// staleness lookback, so a counter that stopped incrementing more than 5
	// minutes before To (failures that clustered early in the window) would
	// vanish at the boundary and be undercounted — memq has no such limit. The
	// window range looks back far enough to find the boundary's latest sample.
	//
	// A group present at To but absent at From (a series that first appeared
	// inside the window) must count its full end value, not be dropped by the
	// subtraction — memq starts such a counter from 0. The `or (<end> * 0)`
	// fills every end-group's missing start value with 0, so `A - (B or A*0)`
	// never silently drops a one-end series.
	rng := promDuration(q.Range.To.Sub(q.Range.From))
	end := fmt.Sprintf("sum %s(last_over_time(%s%s[%s] @ %s))", by, q.Metric, matchers, rng, promTime(evalTo))
	start := fmt.Sprintf("sum %s(last_over_time(%s%s[%s] @ %s))", by, q.Metric, matchers, rng, promTime(evalFrom))
	return promExpr{
		expr: fmt.Sprintf("%s - (%s or (%s * 0))", end, start, end),
		at:   at,
	}, nil
}

// labelMatchers renders sorted PromQL label matchers, e.g. {flow="invoice.pay",stage="capture"}.
func labelMatchers(filters map[string]string) string {
	if len(filters) == 0 {
		return ""
	}
	keys := make([]string, 0, len(filters))
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, filters[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// groupBy renders a sorted `by (a, b)` clause, or "" when empty.
func groupBy(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	return "by (" + strings.Join(sorted, ", ") + ") "
}

// promDuration renders a duration in PromQL form (whole seconds), e.g. "1800s".
// Seconds keep sub-minute ranges exact and avoid float minute/hour formatting.
func promDuration(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%ds", secs)
}

// promTime renders an @-modifier timestamp as unix seconds with millis.
func promTime(t time.Time) string {
	return fmt.Sprintf("%.3f", float64(t.UnixNano())/1e9)
}
