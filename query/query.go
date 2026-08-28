// Package query defines the only questions the engine may ask a backend:
// sum, count, group-by, and time range over metrics, and filter, group-by,
// order and distinct-count over events. Query adapters translate this
// AST; nothing vendor-specific crosses it. If an engine change appears to
// need a new Querier method, that is a design smell to raise, not a
// method to add.
//
// FROZEN SURFACE (v0.1.0).
package query

import (
	"context"
	"errors"
	"time"
)

// Agg is the aggregation applied to a metric query.
type Agg string

const (
	AggSum   Agg = "sum"
	AggCount Agg = "count"
)

// TimeRange is a half-open window [From, To).
type TimeRange struct {
	From time.Time
	To   time.Time
}

// Query asks for an aggregated metric over a range. Filters and GroupBy
// name ADR-0004 labels only.
//
// Temporal semantics every adapter MUST implement identically — two
// conforming adapters returning different numbers for the same Query is
// cross-backend drift in the numbers Finance reads:
//   - Counter families: each returned Point is the INCREASE within its
//     step interval (PromQL: sum(increase(m[step]))); never a cumulative
//     sample.
//   - Gauge families: each Point is the last observed level at its step
//     boundary.
//   - Step == 0 means one bucket spanning the whole Range (a single
//     Point per series).
type Query struct {
	Metric  string
	Agg     Agg
	Filters map[string]string
	GroupBy []string
	Range   TimeRange
	Step    time.Duration
}

// EventAgg selects what an event query computes.
type EventAgg string

const (
	// EventAggGroups returns one EventGroup per distinct key (the
	// default, zero value).
	EventAggGroups EventAgg = ""
	// EventAggDistinctCount returns a single EventGroup whose Count is
	// the number of distinct GroupBy key combinations — the customers
	// leg's distinct count without an unbounded fetch (SQL
	// COUNT(DISTINCT), CW Insights count_distinct, SPL dc()).
	EventAggDistinctCount EventAgg = "distinct_count"
	// EventAggMaxPerGroup returns one group per distinct key (like
	// EventAggGroups) and additionally sets EventGroup.MaxMinor to the
	// MAXIMUM single event's minor amount in the group. It lets the engine
	// take one representative amount per entity for exact de-dup instead of
	// averaging SumMinor/Count (ADR-0009). MaxMinor is money, so the same
	// currency invariant as EventAggGroups applies.
	EventAggMaxPerGroup EventAgg = "max_per_group"
)

// EventOrder fixes which groups a Limit keeps. Limit without an order
// would mean a DIFFERENT arbitrary N per backend.
type EventOrder string

const (
	// OrderNone: no ordering contract; Limit MUST be 0.
	OrderNone EventOrder = ""
	// OrderSumDesc: groups ordered by SumMinor descending (top accounts
	// by value).
	OrderSumDesc EventOrder = "sum_desc"
	// OrderCountDesc: groups ordered by Count descending.
	OrderCountDesc EventOrder = "count_desc"
)

// EventQuery asks for grouped outcome events. Money invariant: when Agg
// reads money per group (EventAggGroups' SumMinor or EventAggMaxPerGroup's
// MaxMinor), "currency" MUST be in GroupBy or pinned by Filters — adapters
// MUST reject a result that would compare across currencies (ADR-0001 forbids
// silent normalization, and five adapter authors deciding independently is how
// it would happen).
type EventQuery struct {
	Filters map[string]string
	GroupBy []string
	Range   TimeRange
	Agg     EventAgg
	OrderBy EventOrder
	Limit   int
}

// Point is one sample. Value is float64 because that is what time-series
// backends store; the engine owns the care needed when reading money out
// of a TSDB (events remain the exact source of truth).
type Point struct {
	At    time.Time
	Value float64
}

// SeriesSlice is one labeled series of points.
type SeriesSlice struct {
	Labels map[string]string
	Points []Point
}

// Series is a metric query result.
type Series []SeriesSlice

// EventGroup is one group of outcome events: the group key, how many, and
// their exact minor-unit sum. SumMinor is meaningful only under the
// EventQuery currency invariant above.
type EventGroup struct {
	Key      map[string]string
	Count    int64
	SumMinor int64
	// MaxMinor is the maximum single event's minor amount in the group. It is
	// populated ONLY when EventQuery.Agg == EventAggMaxPerGroup (zero
	// otherwise) and, being money, is meaningful only under the same currency
	// invariant as SumMinor. See ADR-0009.
	MaxMinor int64
}

// EventGroups is an event query result, in the EventQuery's OrderBy
// order (unspecified order when OrderNone).
type EventGroups []EventGroup

// Caps declares what a backend can honestly serve. Metrics and events
// are independent capabilities (Loki-style log stores serve events but
// no metric TSDB; Prometheus serves metrics but no events), and their
// retentions routinely differ — the baseline leg reads
// MetricHistoryWeeks, the customers leg reads EventHistoryWeeks.
// emit.Caps is the write-side mirror of this shape — amend BOTH together
// (a parity test in each package pins the tie).
type Caps struct {
	Metrics            bool
	Events             bool
	MetricHistoryWeeks int
	EventHistoryWeeks  int
}

// ErrUnsupported is returned by EITHER verb a backend cannot serve:
// QueryEvents on a metrics-only backend, QueryMetric on an events-only
// one. The engine MUST turn it into an honest NotAvailable, never a
// silent zero.
var ErrUnsupported = errors.New("query: this signal kind is not supported by this backend")

// Querier is the whole read boundary.
type Querier interface {
	QueryMetric(ctx context.Context, q Query) (Series, error)
	QueryEvents(ctx context.Context, q EventQuery) (EventGroups, error)
	Capabilities() Caps
}
