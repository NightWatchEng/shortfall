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
var gaugeFamilies = map[string]bool{"biz_inflight_value": true}

// promExpr is a translated PromQL query plus how to evaluate it.
type promExpr struct {
	expr    string
	instant bool          // true: one bucket over the range, evaluate at `at`
	at      time.Time     // instant evaluation time (Range.To)
	start   time.Time     // range start (step queries)
	end     time.Time     // range end (step queries)
	step    time.Duration // range step (step queries)
}

// translate turns a frozen query.Query into PromQL matching the memq temporal
// semantics: counter families sum the increase within each step interval;
// the gauge family reads the last level at each step boundary; Step==0 is one
// bucket over the whole range, evaluated at Range.To as an instant query.
func translate(q query.Query) (promExpr, error) {
	if q.Agg == query.AggCount {
		return promExpr{}, fmt.Errorf("promql: AggCount is not supported; the engine reads counts as AggSum over biz_txn_total")
	}
	matchers := labelMatchers(q.Filters)
	by := groupBy(q.GroupBy)
	gauge := gaugeFamilies[q.Metric]

	if q.Step <= 0 {
		// One bucket over [From, To): instant query at To.
		rng := q.Range.To.Sub(q.Range.From)
		var inner string
		if gauge {
			inner = q.Metric + matchers // last level at To
		} else {
			inner = fmt.Sprintf("increase(%s%s[%s])", q.Metric, matchers, promDuration(rng))
		}
		return promExpr{
			expr:    fmt.Sprintf("sum %s(%s)", by, inner),
			instant: true,
			at:      q.Range.To,
		}, nil
	}

	// Stepped range query.
	var inner string
	if gauge {
		inner = q.Metric + matchers
	} else {
		inner = fmt.Sprintf("increase(%s%s[%s])", q.Metric, matchers, promDuration(q.Step))
	}
	return promExpr{
		expr:  fmt.Sprintf("sum %s(%s)", by, inner),
		start: q.Range.From,
		end:   q.Range.To,
		step:  q.Step,
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
// Seconds keep sub-minute ranges/steps exact and avoid float minute/hour
// formatting.
func promDuration(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%ds", secs)
}
