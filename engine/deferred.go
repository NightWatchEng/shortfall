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

// Deferred computes the in-flight (deferred) value leg from biz_inflight_value
// at the window's snapshot instant. Deferred is not lost — the leg's point is
// the distinction: money still moving, some of it past its SLA and projected
// to become lost, most of it not.
//
// ByAgeBucket and ByCurrency are exact gauge reads. ProjectedLostMinor is a
// conservative lower bound: it sums value in buckets entirely past a stage's
// SLA deadline (bucket-floor granularity), so a deadline falling inside a
// bucket under-attributes that bucket. OldestAgeMinutes is the floor age of
// the oldest non-empty bucket, also a lower bound. Leg.Count and SLABreaches
// come from the companion biz_inflight_count gauge (ADR-0012); a value-only
// source leaves them 0 with a caveat.
//
// Evidence is deterministic (a measured level). A backend with no metric
// source cannot ground this leg and Deferred returns an error.
func Deferred(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (DeferredLeg, error) {
	if !q.Capabilities().Metrics {
		return DeferredLeg{}, fmt.Errorf("engine: deferred leg needs a metric source (biz_inflight_value); this backend serves no metrics")
	}

	leg := DeferredLeg{
		Leg: Leg{
			ByCurrency: map[string]int64{},
			Evidence:   EvidenceDeterministic,
		},
		ByAgeBucket:        map[string]map[string]int64{},
		ProjectedLostMinor: map[string]int64{},
	}
	oldestIdx := -1

	for _, filters := range inflightFilters(req) {
		series, err := q.QueryMetric(ctx, query.Query{
			Metric:  "biz_inflight_value",
			Filters: filters,
			GroupBy: []string{"flow", "stage", "age_bucket", "currency"},
			Range:   req.Window,
		})
		if err != nil {
			return DeferredLeg{}, fmt.Errorf("engine: deferred inflight query: %w", err)
		}

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
		return DeferredLeg{}, err
	}

	return leg, nil
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
