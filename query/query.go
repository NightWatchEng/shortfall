// Package query defines the only questions the engine may ask a backend:
// sum, count, group-by, and time range over metrics, and filter plus
// group-by over events. Query adapters translate this AST; nothing
// vendor-specific crosses it. If an engine change appears to need a new
// Querier method, that is a design smell to raise, not a method to add.
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
type Query struct {
	Metric  string
	Agg     Agg
	Filters map[string]string
	GroupBy []string
	Range   TimeRange
	Step    time.Duration
}

// EventQuery asks for grouped outcome events.
type EventQuery struct {
	Filters map[string]string
	GroupBy []string
	Range   TimeRange
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
// their exact minor-unit sum.
type EventGroup struct {
	Key      map[string]string
	Count    int64
	SumMinor int64
}

// EventGroups is an event query result.
type EventGroups []EventGroup

// Caps mirrors emit.Caps on the read side.
type Caps struct {
	Events       bool
	HistoryWeeks int
}

// ErrUnsupported is returned by QueryEvents on metrics-only backends. The
// engine turns it into an honest NotAvailable, never a silent zero.
var ErrUnsupported = errors.New("query: events are not supported by this backend")

// Querier is the whole read boundary.
type Querier interface {
	QueryMetric(ctx context.Context, q Query) (Series, error)
	QueryEvents(ctx context.Context, q EventQuery) (EventGroups, error)
	Capabilities() Caps
}
