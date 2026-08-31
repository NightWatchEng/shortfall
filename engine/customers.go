// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// Customers computes who was hit: distinct affected accounts, a per-segment
// breakdown, and the top-N by failed value — exportable for credits and
// outreach. When the backend cannot serve events (a metrics-only TSDB), the
// leg says why it is unavailable rather than rendering a misleading zero.
//
// Groups are queried per (currency, customer, segment) — sums never cross a
// currency (ADR-0001) — and merged per customer, so Distinct and the segment
// counts are exact even across flows (a customer hit in two flows is one
// account).
//
// The leg is recovery-agnostic: ByCurrency is the gross value of
// a customer's failed transactions, not netted for later recoveries — outreach
// wants everyone who experienced a failure. Net-of-recovery loss is the
// realized leg's job; do not sum this leg's ByCurrency as company loss.
//
// Top-N ranking orders accounts by their largest single-currency failed value,
// never a cross-currency total (ADR-0001); ties break by customer id for
// determinism.
func Customers(ctx context.Context, reg *registry.Registry, q query.Querier, req Request, topN int) (CustomersLeg, error) {
	_ = reg // segments are read from the event data, not the registry
	if !q.Capabilities().Events {
		return CustomersLeg{
			NotAvailableReason: "backend serves no events; per-customer impact (distinct, by segment, top-N) is unavailable — pair with an event exporter",
		}, nil
	}

	type acct struct {
		segment string
		byCur   map[string]int64
	}
	accts := map[string]*acct{}

	for _, filters := range flowFilters(req, "failed") {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   req.Window,
			Filters: filters,
			GroupBy: []string{"currency", "customer", "segment"},
		})
		if err != nil {
			return CustomersLeg{}, fmt.Errorf("engine: customers query: %w", err)
		}
		for _, g := range groups {
			id := g.Key["customer"]
			a := accts[id]
			if a == nil {
				a = &acct{segment: g.Key["segment"], byCur: map[string]int64{}}
				accts[id] = a
			}
			a.byCur[g.Key["currency"]] += g.SumMinor
		}
	}

	leg := CustomersLeg{
		Distinct:  int64(len(accts)),
		BySegment: map[string]int64{},
	}
	for _, a := range accts {
		leg.BySegment[a.segment]++
	}

	// Rank by largest single-currency loss (currency-safe), tiebreak by id.
	ids := make([]string, 0, len(accts))
	for id := range accts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		mi, mj := maxCurrency(accts[ids[i]].byCur), maxCurrency(accts[ids[j]].byCur)
		if mi != mj {
			return mi > mj
		}
		return ids[i] < ids[j]
	})
	if topN > 0 && len(ids) > topN {
		ids = ids[:topN]
	}
	for _, id := range ids {
		if topN <= 0 {
			break
		}
		leg.TopN = append(leg.TopN, CustomerImpact{
			CustomerID: id,
			Segment:    accts[id].segment,
			ByCurrency: accts[id].byCur,
		})
	}
	return leg, nil
}

// maxCurrency returns the largest per-currency failed value in the map (the
// currency-safe ranking key — never a cross-currency sum).
func maxCurrency(byCur map[string]int64) int64 {
	var max int64
	first := true
	for _, v := range byCur {
		if first || v > max {
			max, first = v, false
		}
	}
	return max
}
