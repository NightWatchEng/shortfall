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
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// The ADR-0005 age buckets, exported so no caller ever spells one by
// hand — a typo would mint a phantom gauge series past the ADR-0004
// cardinality fence.
const (
	AgeLt1m    = "lt1m"
	Age1mTo5m  = "1m-5m"
	Age5mTo30m = "5m-30m"
	Age30mTo2h = "30m-2h"
	AgeGt2h    = "gt2h"
)

// AgeBuckets lists the five buckets in ascending order.
var AgeBuckets = []string{AgeLt1m, Age1mTo5m, Age5mTo30m, Age30mTo2h, AgeGt2h}

// MetricPoint is one bounded-label metric observation. Name is one of the
// fixed ADR-0004 families; Labels never exceed the family's declared set,
// and never carry entity or customer ids — the emitter implementation
// MUST enforce that when constructing points (cardinality protection is a
// library guarantee, not exporter goodwill).
//
// Semantics: for counter families the Value is a DELTA — the increment
// observed at At — never a cumulative total; for the gauge families
// (biz_inflight_value) it is the level observed at At. At is the
// observation time and travels with the point: batching exporters (EMF,
// Datadog series, remote-write) stamp backends with At, never with
// flush time — a batch delayed by an incident must not move money in
// time.
type MetricPoint struct {
	Name   string
	Labels map[string]string
	// Value is int64 by design: counters count, and value sums are minor
	// units (ADR-0001).
	Value int64
	At    time.Time
}

// Caps declares what an exporter can honestly do. Honesty is
// load-bearing: a metrics-only exporter says Events=false and the engine
// reports the customers leg as unavailable rather than silently empty.
// query.Caps is the read-side mirror of this shape — amend BOTH together
// (a parity test in each package pins the tie).
type Caps struct {
	Metrics            bool
	Events             bool
	MetricHistoryWeeks int
	EventHistoryWeeks  int
}

// Exporter ships the two signal kinds to a backend. Implementations live
// under adapters/ as nested modules and MUST pass the exporter
// conformance suite (landing in testkit with the first exporter, M4):
// batching, flush on Shutdown, capability honesty. Export failure
// semantics are ADR-0002's and bind every implementation: never block
// the caller, drop with a visible counter, never silently.
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
	// At overrides the outcome's event time. Webhook-fed adapters MUST
	// set it from the provider's event timestamp: deliveries arrive
	// hours late during exactly the outages this library measures, and
	// receipt-time stamping would move realized loss across incident
	// windows.
	At time.Time
}

// Emitter is the application-facing surface: one call per stage
// transition, one gauge update path for in-flight value.
//
// Contract (binds the implementation, stated where application authors
// read): Record NEVER blocks the request path and returns no error —
// invalid or overflowing outcomes are dropped and counted on
// biz_dropped_events_total, never silently. Outcome events are emitted
// regardless of trace sampling — money accounting never depends on a
// sampler.
type Emitter interface {
	Record(ctx context.Context, stage string, result biz.Result, opts ...Option)
	SetInFlight(flow, stage, ageBucket string, money biz.Money)
}
