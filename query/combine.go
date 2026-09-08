// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package query

import "context"

// Combine pairs a metrics backend with an events backend as one Querier —
// the common split of the biz_* families in a TSDB and outcome events in a
// log store or SQL table, which no single shipped adapter serves both
// halves of. Each verb reaches only the side that owns its signal, and
// each Capabilities field is taken from that side, so the events store's
// opinion of metric retention never leaks into the baseline leg (nor the
// reverse).
//
// It never falls back: a metrics-only store handed as the events side
// answers QueryEvents with its own ErrUnsupported, and the engine marks
// the leg unavailable exactly as it would for a lone backend. A nil side
// is a wiring bug and panics at construction rather than at the first
// report.
func Combine(metrics, events Querier) Querier {
	if metrics == nil || events == nil {
		panic("query: Combine needs both a metrics and an events Querier")
	}

	return combined{metrics: metrics, events: events}
}

type combined struct {
	metrics Querier
	events  Querier
}

func (c combined) QueryMetric(ctx context.Context, q Query) (Series, error) {
	return c.metrics.QueryMetric(ctx, q)
}

func (c combined) QueryEvents(ctx context.Context, q EventQuery) (EventGroups, error) {
	return c.events.QueryEvents(ctx, q)
}

func (c combined) Capabilities() Caps {
	m, e := c.metrics.Capabilities(), c.events.Capabilities()
	return Caps{
		Metrics:            m.Metrics,
		Events:             e.Events,
		MetricHistoryWeeks: m.MetricHistoryWeeks,
		EventHistoryWeeks:  e.EventHistoryWeeks,
	}
}
