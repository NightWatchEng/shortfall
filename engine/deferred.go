// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// ageBucketFloorMinutes maps each ADR-0005 age bucket to the minimum age (in
// minutes) of the value it holds. An item in a bucket is at least this old.
var ageBucketFloorMinutes = map[string]int64{
	"lt1m":   0,
	"1m-5m":  1,
	"5m-30m": 5,
	"30m-2h": 30,
	"gt2h":   120,
}

// ageBucketOrder is oldest-last, for finding the oldest non-empty bucket.
var ageBucketOrder = []string{"lt1m", "1m-5m", "5m-30m", "30m-2h", "gt2h"}

// Deferred computes the in-flight (deferred) value leg at the window's
// snapshot instant. Deferred is not lost — the leg's point is the
// distinction: money still moving, some of it past its SLA and projected to
// become lost, most of it not.
//
// Two groundings, in a fixed order (ADR-0019). The biz_inflight_value gauge
// is authoritative whenever the backend returns any series for it — a level
// of zero included, because a tracker that observed Done knows about
// completions the event stream may show only outside the window. When no
// gauge series exists and the backend serves events, the leg is derived from
// outcome events instead: an entity with a `deferred` outcome and no terminal
// outcome at the same or a later stage is in flight, valued at its largest
// single deferred amount (ADR-0009), aged from its first deferred event. A
// backend serving neither signal cannot ground the leg and Deferred returns
// an error.
//
// ByAgeBucket and ByCurrency are exact reads on the gauge path and exact
// per-entity sums on the events path. ProjectedLostMinor is a conservative
// lower bound: it sums value in buckets entirely past a stage's SLA deadline
// (bucket-floor granularity), so a deadline falling inside a bucket
// under-attributes that bucket. OldestAgeMinutes is the floor age of the
// oldest non-empty bucket, also a lower bound. On the gauge path Leg.Count
// and SLABreaches come from the companion biz_inflight_count gauge
// (ADR-0012), and a value-only source leaves them 0 with a caveat; on the
// events path both are exact entity counts. Evidence is deterministic either
// way (a measured level, or recorded transactions).
func Deferred(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (DeferredLeg, error) {
	caps := q.Capabilities()
	if !caps.Metrics && !caps.Events {
		return DeferredLeg{}, fmt.Errorf("engine: deferred leg needs the biz_inflight_value gauge or deferred outcome events; this backend serves neither")
	}

	if caps.Metrics {
		leg, sawGauge, err := deferredFromGauge(ctx, reg, q, req)
		if err != nil {
			return DeferredLeg{}, err
		}

		if sawGauge || !caps.Events {
			return leg, nil
		}
	}

	return deferredFromEvents(ctx, reg, q, req)
}

// newDeferredLeg is the empty, deterministic leg both groundings fill.
func newDeferredLeg() DeferredLeg {
	return DeferredLeg{
		Leg: Leg{
			ByCurrency: map[string]int64{},
			Evidence:   EvidenceDeterministic,
		},
		ByAgeBucket:        map[string]map[string]int64{},
		ProjectedLostMinor: map[string]int64{},
	}
}

// deferredFromGauge reads the leg from biz_inflight_value (and the companion
// count gauge). sawGauge reports whether the backend returned ANY in-flight
// series for the scope, at any level — the signal that a tracker is
// publishing and the gauge is the source of record.
func deferredFromGauge(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (leg DeferredLeg, sawGauge bool, err error) {
	leg = newDeferredLeg()
	oldestIdx := -1

	for _, filters := range inflightFilters(req) {
		series, err := q.QueryMetric(ctx, query.Query{
			Metric:  "biz_inflight_value",
			Filters: filters,
			GroupBy: []string{"flow", "stage", "age_bucket", "currency"},
			Range:   req.Window,
		})
		if err != nil {
			return DeferredLeg{}, false, fmt.Errorf("engine: deferred inflight query: %w", err)
		}

		sawGauge = sawGauge || len(series) > 0

		for _, s := range series {
			level := lastLevel(s)
			if level == 0 {
				continue
			}

			bucket := s.Labels["age_bucket"]
			currency := s.Labels["currency"]

			if leg.ByAgeBucket[bucket] == nil {
				leg.ByAgeBucket[bucket] = map[string]int64{}
			}

			leg.ByAgeBucket[bucket][currency] += level
			leg.ByCurrency[currency] += level

			if idx := bucketIndex(bucket); idx > oldestIdx {
				oldestIdx = idx
			}

			// Projected-lost: value in a bucket entirely past the stage's SLA
			// deadline, when the registry says a breach becomes lost.
			if reg != nil && breachedAndLost(reg, s.Labels["flow"], s.Labels["stage"], bucket) {
				leg.ProjectedLostMinor[currency] += level
			}
		}
	}

	if oldestIdx >= 0 {
		leg.OldestAgeMinutes = ageBucketFloorMinutes[ageBucketOrder[oldestIdx]]
	}

	if err := fillDeferredCounts(ctx, reg, q, req, &leg); err != nil {
		return DeferredLeg{}, false, err
	}

	return leg, sawGauge, nil
}

// eventsDerivedCaveat names the events grounding and its two limits on the
// leg it produces.
const eventsDerivedCaveat = "events-derived: no biz_inflight_value gauge grounded this leg, so it is built from deferred outcome events with no later terminal outcome — ages run from the first deferred event, and work never recorded as deferred is not seen (ADR-0019)"

// terminalOutcomes resolve a deferral: the entity's money reached a terminal
// state. `unknown` is not terminal — the money is still unresolved.
var terminalOutcomes = []string{"success", "failed", "abandoned"}

// ageCutoffs are the nested ranges the events path derives age buckets from,
// oldest first: an entity whose first deferred event lies before To-2h is
// gt2h, otherwise before To-30m is 30m-2h, and so on down to lt1m. The
// event AST returns no per-event timestamps, so the age is read from which
// range the entity first appears in — bucket granularity, which is all the
// SLA arithmetic uses anyway (ADR-0005).
var ageCutoffs = []struct {
	bucket string
	before time.Duration
}{
	{"gt2h", 2 * time.Hour},
	{"30m-2h", 30 * time.Minute},
	{"5m-30m", 5 * time.Minute},
	{"1m-5m", time.Minute},
	{"lt1m", 0},
}

// minEventsLookback is how far before the window start the events path
// looks for deferrals still open at the window end, at least: backlog older
// than the window is still backlog.
const minEventsLookback = 2 * time.Hour

// deferredFromEvents derives the leg from outcome events (ADR-0019).
func deferredFromEvents(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (DeferredLeg, error) {
	leg := newDeferredLeg()
	leg.Caveats = []string{eventsDerivedCaveat}
	start := req.Window.From.Add(-eventsLookback(reg, req))

	type entityStage struct{ flow, stage, currency, entity string }
	type inflightItem struct {
		bucket string // fixed by the first (oldest) range the entity appears in
		minor  int64  // the largest single deferred amount (ADR-0009)
	}
	inflight := map[entityStage]inflightItem{}
	for _, cut := range ageCutoffs {
		rng := query.TimeRange{From: start, To: req.Window.To.Add(-cut.before)}
		if !rng.To.After(rng.From) {
			continue
		}

		for _, filters := range flowFilters(req, "deferred") {
			groups, err := q.QueryEvents(ctx, query.EventQuery{
				Range: rng, Filters: filters,
				GroupBy: []string{"flow", "stage", "currency", "entity"},
				Agg:     query.EventAggMaxPerGroup,
			})
			if err != nil {
				return DeferredLeg{}, fmt.Errorf("engine: deferred events query: %w", err)
			}

			for _, g := range groups {
				k := entityStage{g.Key["flow"], g.Key["stage"], g.Key["currency"], g.Key["entity"]}
				item, seen := inflight[k]
				if !seen {
					item.bucket = cut.bucket
				}

				if g.MaxMinor > item.minor {
					item.minor = g.MaxMinor
				}

				inflight[k] = item
			}
		}
	}

	// A terminal outcome at the same stage, or at any later stage of the
	// flow, resolves the deferral — and so does a deferral at a later stage,
	// because entering the next queue means the earlier one released the
	// transaction. Stages the registry does not order resolve only their own
	// deferrals.
	type flowEntity struct{ flow, entity string }
	terminalStages := map[flowEntity]map[string]bool{}
	laterDeferred := map[flowEntity]map[string]bool{}
	for k := range inflight {
		fe := flowEntity{k.flow, k.entity}
		if laterDeferred[fe] == nil {
			laterDeferred[fe] = map[string]bool{}
		}

		laterDeferred[fe][k.stage] = true
	}

	for _, outcome := range terminalOutcomes {
		for _, filters := range flowFilters(req, outcome) {
			groups, err := q.QueryEvents(ctx, query.EventQuery{
				Range: query.TimeRange{From: start, To: req.Window.To}, Filters: filters,
				GroupBy: []string{"flow", "stage", "currency", "entity"},
			})
			if err != nil {
				return DeferredLeg{}, fmt.Errorf("engine: deferred terminal query: %w", err)
			}

			for _, g := range groups {
				fe := flowEntity{g.Key["flow"], g.Key["entity"]}
				if terminalStages[fe] == nil {
					terminalStages[fe] = map[string]bool{}
				}

				terminalStages[fe][g.Key["stage"]] = true
			}
		}
	}

	oldestIdx := -1
	for k, item := range inflight {
		fe := flowEntity{k.flow, k.entity}
		if resolvedByLaterStage(reg, k.flow, k.stage, terminalStages[fe]) || progressedPast(reg, k.flow, k.stage, laterDeferred[fe]) {
			continue
		}

		if leg.ByAgeBucket[item.bucket] == nil {
			leg.ByAgeBucket[item.bucket] = map[string]int64{}
		}

		leg.ByAgeBucket[item.bucket][k.currency] += item.minor
		leg.ByCurrency[k.currency] += item.minor
		leg.Count++
		if idx := bucketIndex(item.bucket); idx > oldestIdx {
			oldestIdx = idx
		}

		if reg == nil {
			continue
		}

		if breached(reg, k.flow, k.stage, item.bucket) {
			leg.SLABreaches++
		}

		if breachedAndLost(reg, k.flow, k.stage, item.bucket) {
			leg.ProjectedLostMinor[k.currency] += item.minor
		}
	}

	if oldestIdx >= 0 {
		leg.OldestAgeMinutes = ageBucketFloorMinutes[ageBucketOrder[oldestIdx]]
	}

	return leg, nil
}

// resolvedByLaterStage reports whether any of the entity's terminal stages
// is the deferred stage itself or a later stage in the flow's registry order.
func resolvedByLaterStage(reg *registry.Registry, flow, stage string, terminals map[string]bool) bool {
	if terminals[stage] {
		return true
	}

	deferredIdx := stageIndex(reg, flow, stage)
	if deferredIdx < 0 {
		return false
	}

	for t := range terminals {
		if idx := stageIndex(reg, flow, t); idx > deferredIdx {
			return true
		}
	}

	return false
}

// progressedPast reports whether the entity was deferred again at a stage
// later in the flow's registry order than stage: it left this queue for the
// next one, so this deferral is resolved and the later one carries the money.
func progressedPast(reg *registry.Registry, flow, stage string, deferredStages map[string]bool) bool {
	idx := stageIndex(reg, flow, stage)
	if idx < 0 {
		return false
	}

	for d := range deferredStages {
		if stageIndex(reg, flow, d) > idx {
			return true
		}
	}

	return false
}

// stageIndex is the stage's position in the flow's declared order, or -1
// when the registry does not order it.
func stageIndex(reg *registry.Registry, flow, stage string) int {
	if reg == nil {
		return -1
	}

	f, ok := reg.Flow(flow)
	if !ok {
		return -1
	}

	for i, st := range f.Stages {
		if st.Name == stage {
			return i
		}
	}

	return -1
}

// eventsLookback is how far before the window start the events path looks
// for still-open deferrals: at least minEventsLookback, and at least the
// longest SLA deadline of the flows in scope, so a backlog that has been
// breaching for a day is still seen.
func eventsLookback(reg *registry.Registry, req Request) time.Duration {
	lookback := minEventsLookback
	if reg == nil {
		return lookback
	}

	flows := req.Flows
	if len(flows) == 0 {
		flows = reg.FlowNames()
	}

	for _, name := range flows {
		f, ok := reg.Flow(name)
		if !ok {
			continue
		}

		for _, sla := range f.SLA {
			if sla.Deadline > lookback {
				lookback = sla.Deadline
			}
		}
	}

	return lookback
}

// fillDeferredCounts reads biz_inflight_count to fill Leg.Count (all buckets)
// and SLABreaches (breaching buckets). If the count gauge is absent while value
// is present, it records the count-unavailable caveat instead of asserting 0.
func fillDeferredCounts(ctx context.Context, reg *registry.Registry, q query.Querier, req Request, leg *DeferredLeg) error {
	sawCount := false
	for _, filters := range inflightFilters(req) {
		series, err := q.QueryMetric(ctx, query.Query{
			Metric:  "biz_inflight_count",
			Filters: filters,
			GroupBy: []string{"flow", "stage", "age_bucket", "currency"},
			Range:   req.Window,
		})
		if err != nil {
			return fmt.Errorf("engine: deferred inflight count query: %w", err)
		}

		for _, s := range series {
			count := lastLevel(s)
			if count == 0 {
				continue
			}

			sawCount = true
			leg.Count += count
			// SLABreaches counts every transaction past its SLA deadline —
			// at_risk breaches included, not just the "lost" ones.
			if reg != nil && breached(reg, s.Labels["flow"], s.Labels["stage"], s.Labels["age_bucket"]) {
				leg.SLABreaches += count
			}
		}
	}

	// Caveat only when in-flight value exists but no count gauge counted it —
	// an older, value-only source. No in-flight at all needs no caveat.
	if !sawCount && len(leg.ByAgeBucket) > 0 {
		leg.Caveats = append(leg.Caveats,
			"in-flight and SLA-breach transaction COUNTS are unavailable — this source emits biz_inflight_value but not biz_inflight_count (ADR-0012); breach is reported as projected-lost value")
	}

	return nil
}

// lastLevel returns the gauge level for a series (the sum of its points;
// with Step 0 there is exactly one carried-forward level point).
func lastLevel(s query.SeriesSlice) int64 {
	var v int64
	for _, p := range s.Points {
		v += int64(p.Value)
	}

	return v
}

// bucketIndex returns a bucket's position in oldest-last order, or -1.
func bucketIndex(bucket string) int {
	for i, b := range ageBucketOrder {
		if b == bucket {
			return i
		}
	}

	return -1
}

// breached reports whether a bucket is entirely past the flow/stage SLA
// deadline — regardless of the on_breach policy. It is the predicate for the
// SLA-breach transaction count: an at_risk breach is still a breach.
//
// The bucket is entirely past the deadline when its floor age already meets
// it, compared as durations: truncating a fractional-minute deadline (PT90S)
// to whole minutes would pull earlier buckets over the line and over-state
// breaches. A deadline falling inside a bucket is not attributed to that
// bucket (a documented lower bound).
func breached(reg *registry.Registry, flow, stage, bucket string) bool {
	f, ok := reg.Flow(flow)
	if !ok {
		return false
	}

	sla, ok := f.SLA[stage]
	if !ok {
		return false
	}

	floorMin, ok := ageBucketFloorMinutes[bucket]
	if !ok {
		return false
	}

	return sla.Deadline <= time.Duration(floorMin)*time.Minute
}

// breachedAndLost is breached and the registry's on_breach policy is "lost" —
// the predicate for projected-lost value (an at_risk breach is a breach but
// not projected loss).
func breachedAndLost(reg *registry.Registry, flow, stage, bucket string) bool {
	f, ok := reg.Flow(flow)
	if !ok {
		return false
	}

	if sla, ok := f.SLA[stage]; !ok || sla.OnBreach != registry.BreachLost {
		return false
	}

	return breached(reg, flow, stage, bucket)
}

// inflightFilters returns one filter map per flow (scope + flow), with no
// outcome — biz_inflight_value has no outcome label.
func inflightFilters(req Request) []map[string]string {
	scope := make(map[string]string, len(req.Scope))
	for k, v := range req.Scope {
		scope[k] = v
	}

	if len(req.Flows) == 0 {
		return []map[string]string{scope}
	}

	out := make([]map[string]string, 0, len(req.Flows))
	for _, f := range req.Flows {
		m := make(map[string]string, len(scope)+1)
		for k, v := range scope {
			m[k] = v
		}

		m["flow"] = f
		out = append(out, m)
	}

	return out
}
