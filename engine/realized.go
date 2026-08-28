package engine

import (
	"context"
	"fmt"

	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// RealizedLeg computes realized loss: value that terminally failed in the
// window and scope, de-duplicated so retries and cross-process duplicate
// events never double-count.
//
// Events path (preferred): for each flow it counts each failed entity once
// and excludes any entity that also has a success in the window — a failed-
// then-recovered transaction is not a loss. Grouping by (currency, entity)
// collapses duplicate failed events for one entity and keeps sums within a
// single currency (ADR-0001).
//
// De-dup and its limit, stated honestly. Under at-least-once delivery a
// duplicate failed event is the SAME event redelivered, so its amount is
// identical and collapsing the group as SumMinor/Count is exact. But the
// frozen query.EventQuery returns only a per-group sum and count — not a
// representative value — so when an entity legitimately has failed events
// with DIFFERING amounts, the collapse yields their mean rather than a real
// figure. The leg flags the DETECTABLE case (the group sum is not divisible
// by its count) with a caveat; the even-division case (e.g. 100+300 over two
// events) is not detectable through a sum-only query and is a documented
// limitation. Exact per-entity de-dup independent of amount uniformity needs
// a max/first-per-group query capability — a frozen-interface amendment
// tracked in workspace-7y5, not worked around here. There is no "entity id
// is unique per transaction" invariant in the library; do not assume one.
//
// Metrics-only path (fallback, when the backend serves no events): the failed
// biz_value_total sums are real, but a time-series cannot de-dup by entity,
// so the leg is labelled "upper bound, not de-duped" — the caveat rides the
// Report's Leg.Caveats, not just prose.
//
// Either way the evidence is deterministic (a measured sum, never an
// estimate). A backend that serves neither events nor metrics cannot ground
// this leg, and RealizedLeg returns an error rather than a plausible zero.
func RealizedLeg(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (Leg, error) {
	_ = reg // reserved: flow validation lands with the Compute assembly
	caps := q.Capabilities()
	switch {
	case caps.Events:
		return realizedFromEvents(ctx, q, req)
	case caps.Metrics:
		return realizedFromMetrics(ctx, q, req)
	default:
		return Leg{}, fmt.Errorf("engine: realized leg needs an event or metric source; this backend serves neither")
	}
}

// flowFilters returns one filter map per flow in the request (or a single
// scope-only filter when no flow is named), each carrying the given outcome.
// Scope is applied FIRST and the reserved keys (outcome, flow) are set after,
// so a stray Scope entry for either cannot override the leg's own filter.
func flowFilters(req Request, outcome string) []map[string]string {
	scope := make(map[string]string, len(req.Scope))
	for k, v := range req.Scope {
		scope[k] = v
	}
	if len(req.Flows) == 0 {
		scope["outcome"] = outcome
		return []map[string]string{scope}
	}
	out := make([]map[string]string, 0, len(req.Flows))
	for _, f := range req.Flows {
		m := make(map[string]string, len(scope)+2)
		for k, v := range scope {
			m[k] = v
		}
		m["outcome"] = outcome
		m["flow"] = f
		out = append(out, m)
	}
	return out
}

func realizedFromEvents(ctx context.Context, q query.Querier, req Request) (Leg, error) {
	leg := Leg{ByCurrency: map[string]int64{}, Evidence: EvidenceDeterministic}

	// Entities that succeeded anywhere in scope recovered — collect them
	// first so a failed-then-succeeded entity is excluded from the loss.
	succeeded := map[string]bool{}
	for _, filters := range flowFilters(req, recoverySuccess) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range: req.Window, Filters: filters, GroupBy: []string{"currency", "entity"},
		})
		if err != nil {
			return Leg{}, fmt.Errorf("engine: realized success query: %w", err)
		}
		for _, g := range groups {
			succeeded[g.Key["entity"]] = true
		}
	}

	// Collapse duplicate failed events per entity to one amount. For a
	// redelivered (identical) duplicate this mean is exact; when a group's
	// sum is not divisible by its count the duplicates carry differing
	// amounts, which the mean cannot represent — flag those loudly. (The
	// even-division differing-amount case is undetectable through a sum-only
	// query; see the package doc and workspace-7y5.)
	inconsistent := 0
	for _, filters := range flowFilters(req, "failed") {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range: req.Window, Filters: filters, GroupBy: []string{"currency", "entity"},
		})
		if err != nil {
			return Leg{}, fmt.Errorf("engine: realized failed query: %w", err)
		}
		for _, g := range groups {
			if succeeded[g.Key["entity"]] {
				continue // recovered on a later attempt
			}
			amount := g.SumMinor
			if g.Count > 1 {
				if g.SumMinor%g.Count != 0 {
					inconsistent++
				}
				amount = g.SumMinor / g.Count // exact under the precondition
			}
			leg.ByCurrency[g.Key["currency"]] += amount
			leg.Count++
		}
	}
	if inconsistent > 0 {
		leg.Caveats = append(leg.Caveats, fmt.Sprintf(
			"%d entity/entities had duplicate failed events with inconsistent amounts; their realized value is approximate", inconsistent))
	}
	return leg, nil
}

func realizedFromMetrics(ctx context.Context, q query.Querier, req Request) (Leg, error) {
	leg := Leg{
		ByCurrency: map[string]int64{},
		Evidence:   EvidenceDeterministic,
		Caveats:    []string{"metrics-only: upper bound, not de-duped by entity"},
	}
	for _, filters := range flowFilters(req, "failed") {
		value, err := q.QueryMetric(ctx, query.Query{
			Metric: "biz_value_total", Agg: query.AggSum, Filters: filters,
			GroupBy: []string{"currency"}, Range: req.Window,
		})
		if err != nil {
			return Leg{}, fmt.Errorf("engine: realized value metric: %w", err)
		}
		for _, s := range value {
			for _, p := range s.Points {
				leg.ByCurrency[s.Labels["currency"]] += int64(p.Value)
			}
		}
		count, err := q.QueryMetric(ctx, query.Query{
			Metric: "biz_txn_total", Agg: query.AggSum, Filters: filters, Range: req.Window,
		})
		if err != nil {
			return Leg{}, fmt.Errorf("engine: realized count metric: %w", err)
		}
		for _, s := range count {
			for _, p := range s.Points {
				leg.Count += int64(p.Value)
			}
		}
	}
	return leg, nil
}

// recoverySuccess is the outcome that cancels a prior failure for an entity.
const recoverySuccess = "success"
