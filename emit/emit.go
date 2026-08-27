// Package emit turns stage transitions into the two normalized signals:
// bounded metrics (sums and counts with the fixed ADR-0004 label set) and
// unsampled per-transaction outcome events. It never touches a backend
// directly; exporters do.
//
// FROZEN SURFACE (v0.1.0): the types and interfaces here are the contract
// adapters implement. Changes after the freeze are interface amendments —
// their own reviewed PRs, never side effects.
package emit

import (
	"context"

	"github.com/NightWatchEng/shortfall/biz"
)

// MetricPoint is one bounded-label metric increment or gauge sample.
// Name is one of the fixed ADR-0004 families; Labels never exceed the
// family's declared set, and never carry entity or customer ids —
// cardinality protection is a library guarantee, enforced by the emitter
// that constructs these, not by exporter goodwill.
type MetricPoint struct {
	Name   string
	Labels map[string]string
	// Value is int64 by design: counters count, and value sums are minor
	// units (ADR-0001). Gauge families (biz_inflight_value) also carry
	// minor units.
	Value int64
}

// Caps declares what an exporter (or querier) can honestly do. Honesty is
// load-bearing: a metrics-only exporter says Events=false and the engine
// reports the customers leg as unavailable rather than silently empty.
type Caps struct {
	Events       bool
	HistoryWeeks int
}

// Exporter ships the two signal kinds to a backend. Implementations live
// under adapters/ as nested modules; the conformance suite in testkit is
// the behavioral contract (batching, flush on Shutdown, capability
// honesty). Export failure semantics are ADR-0002's: never block the
// caller, drop with a visible counter, never silently.
type Exporter interface {
	ExportMetrics(ctx context.Context, batch []MetricPoint) error
	ExportEvents(ctx context.Context, batch []biz.Outcome) error
	Capabilities() Caps
	Shutdown(ctx context.Context) error
}

// Option adjusts one Record call. Concrete options arrive with the
// emitter implementation; the type is frozen so signatures never move.
type Option func(*RecordConfig)

// RecordConfig is the mutable bag Options write into.
type RecordConfig struct {
	// Source overrides the outcome's Source field (e.g. "stripe:webhook").
	Source string
	// Err carries a short failure description onto the outcome.
	Err string
}

// Emitter is the application-facing surface: one call per stage
// transition, one gauge update path for in-flight value. Outcome events
// are emitted regardless of trace sampling — money accounting never
// depends on a sampler.
type Emitter interface {
	Record(ctx context.Context, stage string, result biz.Result, opts ...Option)
	SetInFlight(flow, stage, ageBucket string, money biz.Money)
}
