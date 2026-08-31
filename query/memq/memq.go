// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package memq is an in-memory query.Querier: it answers the frozen query
// AST over a fixed set of metric points and outcome events held in memory,
// so the engine (and adapter conformance) can be exercised with zero
// external processes. It is fed either directly (WithMetrics/WithEvents) or
// from the checkout harness via testkit.
//
// It implements the frozen temporal semantics that every real adapter must
// match, so a number the engine reads from memq is the number it must read
// from PromQL or SQL for the same query:
//   - counter families: each Point is the increase within its step interval;
//   - the gauge family (biz_inflight_value): each Point carries every
//     underlying series' last observed level forward to its step boundary and
//     sums them, matching sum by(g)(last_over_time(m)) — so a GroupBy that
//     collapses several series reports their summed level, not one sample;
//   - Step == 0 means one bucket over the whole Range.
package memq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/query"
)

// gaugeFamilies are the metric names read as a level rather than a delta.
var gaugeFamilies = map[string]bool{"biz_inflight_value": true, "biz_inflight_count": true}

// Querier serves QueryMetric/QueryEvents from in-memory data.
type Querier struct {
	metrics []emit.MetricPoint
	events  []biz.Outcome
	caps    query.Caps
}

var _ query.Querier = (*Querier)(nil)

// Option configures a Querier.
type Option func(*Querier)

// WithMetrics seeds the metric points QueryMetric reads.
func WithMetrics(pts []emit.MetricPoint) Option { return func(q *Querier) { q.metrics = pts } }

// WithEvents seeds the outcome events QueryEvents reads.
func WithEvents(evs []biz.Outcome) Option { return func(q *Querier) { q.events = evs } }

// WithCaps overrides the declared capabilities. Use it to exercise the
// engine's honest NotAvailable paths (e.g. Metrics:false makes QueryMetric
// return query.ErrUnsupported).
func WithCaps(c query.Caps) Option { return func(q *Querier) { q.caps = c } }

// New builds an in-memory querier. Default capabilities serve both signals
// with effectively unbounded history.
func New(opts ...Option) *Querier {
	q := &Querier{caps: query.Caps{
		Metrics:            true,
		Events:             true,
		MetricHistoryWeeks: 520,
		EventHistoryWeeks:  520,
	}}
	for _, o := range opts {
		o(q)
	}

	return q
}

// Capabilities reports the declared capabilities.
func (q *Querier) Capabilities() query.Caps { return q.caps }

// inRange reports whether t is within the half-open window [From, To).
func inRange(t time.Time, r query.TimeRange) bool {
	return !t.Before(r.From) && t.Before(r.To)
}

// matchFilters reports whether every filter key=value is present in labels.
func matchFilters(labels map[string]string, filters map[string]string) bool {
	for k, v := range filters {
		if labels[k] != v {
			return false
		}
	}

	return true
}

// groupKey projects labels onto the GroupBy keys (missing key → "").
func groupKey(labels map[string]string, groupBy []string) map[string]string {
	key := make(map[string]string, len(groupBy))
	for _, k := range groupBy {
		key[k] = labels[k]
	}

	return key
}

// canonical renders a key map as a stable string for grouping/sorting.
func canonical(key map[string]string) string {
	ks := make([]string, 0, len(key))
	for k := range key {
		ks = append(ks, k)
	}

	sort.Strings(ks)
	s := ""
	for _, k := range ks {
		s += k + "=" + key[k] + "\x00"
	}

	return s
}

// QueryMetric aggregates the seeded metric points per the frozen semantics.
func (q *Querier) QueryMetric(_ context.Context, qy query.Query) (query.Series, error) {
	if !q.caps.Metrics {
		return nil, query.ErrUnsupported
	}

	// Collect matching points, grouped by the GroupBy label projection.
	type bucketed struct {
		labels map[string]string
		points []emit.MetricPoint
	}
	gauge := gaugeFamilies[qy.Metric]
	groups := map[string]*bucketed{}
	order := []string{}
	for _, p := range q.metrics {
		if p.Name != qy.Metric || !matchFilters(p.Labels, qy.Filters) {
			continue
		}

		// A counter is filtered to [From, To). A gauge is a persistent
		// level: a sample set before the window still defines the level
		// inside it, so gauge samples are kept whenever they precede the
		// window's end (the bucket reader carries the last one forward,
		// matching last_over_time).
		if gauge {
			if !p.At.Before(qy.Range.To) {
				continue
			}
		} else if !inRange(p.At, qy.Range) {
			continue
		}

		key := groupKey(p.Labels, qy.GroupBy)
		ck := canonical(key)
		g, ok := groups[ck]
		if !ok {
			g = &bucketed{labels: key}
			groups[ck] = g
			order = append(order, ck)
		}

		g.points = append(g.points, p)
	}

	sort.Strings(order)

	out := make(query.Series, 0, len(order))
	for _, ck := range order {
		g := groups[ck]
		out = append(out, query.SeriesSlice{
			Labels: g.labels,
			Points: bucketPoints(g.points, qy, gauge),
		})
	}

	return out, nil
}

// bucketPoints buckets a group's points by Step and aggregates each bucket.
func bucketPoints(points []emit.MetricPoint, qy query.Query, gauge bool) []query.Point {
	// Bucket boundaries.
	if qy.Step <= 0 {
		v := aggregate(points, qy.Range.From, qy.Range.To, qy.Agg, gauge)
		if v == nil {
			return nil
		}

		return []query.Point{{At: qy.Range.From, Value: *v}}
	}

	var out []query.Point
	for start := qy.Range.From; start.Before(qy.Range.To); start = start.Add(qy.Step) {
		end := start.Add(qy.Step)
		if end.After(qy.Range.To) {
			end = qy.Range.To
		}

		if v := aggregate(points, start, end, qy.Agg, gauge); v != nil {
			out = append(out, query.Point{At: start, Value: *v})
		}
	}

	return out
}

// aggregate reduces a group's points to a single bucket value. For a gauge it
// carries each underlying series' last level (most recent sample with At < to)
// forward, then sums those per-series levels — matching a real backend's
// sum by(g)(last_over_time(m)). When GroupBy collapses several series into one
// group, taking a single last sample across the group would return one series'
// level instead of their sum and diverge from Prometheus. Under AggCount the
// gauge value is the number of series with a level; nil only when no level has
// ever been set. For a counter it is the increase within [from, to) (sum of
// delta values), or under AggCount the number of points.
func aggregate(points []emit.MetricPoint, from, to time.Time, agg query.Agg, gauge bool) *float64 {
	if gauge {
		// Per underlying series (full label identity), keep the last level
		// before the boundary.
		type level struct {
			at time.Time
			v  int64
		}
		last := map[string]level{}
		for i := range points {
			p := points[i]
			if !p.At.Before(to) { // carry the last level ≤ boundary forward
				continue
			}

			id := canonical(p.Labels)
			if cur, ok := last[id]; !ok || p.At.After(cur.at) {
				last[id] = level{at: p.At, v: p.Value}
			}
		}

		if len(last) == 0 {
			return nil
		}

		if agg == query.AggCount {
			c := float64(len(last))
			return &c
		}

		var sum float64
		for _, l := range last {
			sum += float64(l.v)
		}

		return &sum
	}

	var sum float64
	var count float64
	seen := false
	for _, p := range points {
		if !p.At.Before(from) && p.At.Before(to) {
			seen = true
			sum += float64(p.Value)
			count++
		}
	}

	if !seen {
		return nil
	}

	if agg == query.AggCount {
		return &count
	}

	return &sum
}

// QueryEvents groups the seeded outcome events per the frozen AST.
func (q *Querier) QueryEvents(_ context.Context, qy query.EventQuery) (query.EventGroups, error) {
	if !q.caps.Events {
		return nil, query.ErrUnsupported
	}

	// Filter to the matched events.
	matched := make([]biz.Outcome, 0, len(q.events))
	for _, o := range q.events {
		labels := eventLabels(o)
		if inRange(o.At, qy.Range) && matchFilters(labels, qy.Filters) {
			matched = append(matched, o)
		}
	}

	if qy.Agg == query.EventAggDistinctCount {
		return distinctCount(matched, qy.GroupBy), nil
	}

	// EventAggGroups / EventAggMaxPerGroup: money is read per group, so enforce
	// the currency invariant (SumMinor and MaxMinor alike, ADR-0001/0009).
	if err := currencyInvariant(matched, qy); err != nil {
		return nil, err
	}

	type agg struct {
		key      map[string]string
		count    int64
		sumMinor int64
		maxMinor int64
	}
	groups := map[string]*agg{}
	order := []string{}
	for _, o := range matched {
		key := groupKey(eventLabels(o), qy.GroupBy)
		ck := canonical(key)
		g, ok := groups[ck]
		if !ok {
			// Seed maxMinor from the first member, not zero: the running max
			// must match SQL's MAX() for negative amounts too.
			g = &agg{key: key, maxMinor: o.VC.Money.Amount}
			groups[ck] = g
			order = append(order, ck)
		}

		g.count++
		g.sumMinor += o.VC.Money.Amount
		if o.VC.Money.Amount > g.maxMinor {
			g.maxMinor = o.VC.Money.Amount
		}
	}

	out := make(query.EventGroups, 0, len(order))
	for _, ck := range order {
		g := groups[ck]
		eg := query.EventGroup{Key: g.key, Count: g.count, SumMinor: g.sumMinor}
		if qy.Agg == query.EventAggMaxPerGroup {
			eg.MaxMinor = g.maxMinor // representative per group (ADR-0009)
		}

		out = append(out, eg)
	}

	if err := orderGroups(out, qy.OrderBy); err != nil {
		return nil, err
	}

	if qy.Limit > 0 {
		if qy.OrderBy == query.OrderNone {
			return nil, errors.New("memq: Limit requires an OrderBy (OrderNone + Limit>0 is undefined)")
		}

		if len(out) > qy.Limit {
			out = out[:qy.Limit]
		}
	}

	return out, nil
}

// distinctCount returns a single group whose Count is the number of distinct
// GroupBy key combinations among the matched events.
func distinctCount(matched []biz.Outcome, groupBy []string) query.EventGroups {
	seen := map[string]bool{}
	for _, o := range matched {
		seen[canonical(groupKey(eventLabels(o), groupBy))] = true
	}

	return query.EventGroups{{Count: int64(len(seen))}}
}

// currencyInvariant rejects money that could cross currencies: when money is
// read per group (EventAggGroups' sum or EventAggMaxPerGroup's max),
// "currency" must be grouped or pinned by a filter.
func currencyInvariant(matched []biz.Outcome, qy query.EventQuery) error {
	if _, pinned := qy.Filters["currency"]; pinned {
		return nil
	}

	for _, g := range qy.GroupBy {
		if g == "currency" {
			return nil
		}
	}

	// Not grouped or pinned: refuse rather than silently normalize (ADR-0001).
	return fmt.Errorf("memq: event sum would cross currencies — pin currency in Filters or add it to GroupBy")
}

// orderGroups sorts groups by the requested order (stable, deterministic).
func orderGroups(g query.EventGroups, o query.EventOrder) error {
	switch o {
	case query.OrderNone:
		sort.SliceStable(g, func(i, j int) bool { return canonical(g[i].Key) < canonical(g[j].Key) })
	case query.OrderSumDesc:
		sort.SliceStable(g, func(i, j int) bool {
			if g[i].SumMinor != g[j].SumMinor {
				return g[i].SumMinor > g[j].SumMinor
			}

			return canonical(g[i].Key) < canonical(g[j].Key)
		})
	case query.OrderCountDesc:
		sort.SliceStable(g, func(i, j int) bool {
			if g[i].Count != g[j].Count {
				return g[i].Count > g[j].Count
			}

			return canonical(g[i].Key) < canonical(g[j].Key)
		})
	default:
		return fmt.Errorf("memq: unknown OrderBy %q", o)
	}

	return nil
}

// eventLabels is the groupable/filterable label view of an outcome. Amounts
// and the raw entity id are addressable for grouping but never summed as a
// label — SumMinor comes from Money.Amount.
func eventLabels(o biz.Outcome) map[string]string {
	return map[string]string{
		"flow":     o.VC.Flow,
		"stage":    o.Stage,
		"outcome":  string(o.Result),
		"currency": o.VC.Money.Currency,
		"segment":  o.VC.Segment,
		"kind":     string(o.VC.Kind),
		"customer": o.VC.CustomerID,
		"entity":   o.VC.EntityID,
	}
}
