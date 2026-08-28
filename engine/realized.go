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
// Events path (preferred, exact): for each flow it sums each failed
// transaction's amount ONCE per entity, and excludes any entity that also
// has a success in the window — a failed-then-recovered transaction is not a
// loss. Grouping by (currency, entity) both de-dups duplicate failed events
// for one entity and keeps sums within a single currency (ADR-0001). The
// per-entity amount is the group sum divided by the group count, which is
// exact under the emitter architecture's guarantee that a cross-process
// duplicate is the same event redelivered (identical amount) and that entity
// ids are unique per transaction; if that guarantee is ever violated the leg
// says so in a caveat rather than passing an average off as a measurement.
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

	// Precondition (emitter architecture): a cross-process duplicate is the
	// SAME event redelivered, and entity ids are unique per transaction, so
	// every failed event for one entity carries an identical amount. Under
	// that guarantee SumMinor/Count is the exact per-entity amount. If it is
	// ever violated (SumMinor not divisible by Count → the duplicates differ),
	// the data is inconsistent: we still emit a best-effort figure but say so
	// loudly rather than pass off an average as an exact measurement.
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
