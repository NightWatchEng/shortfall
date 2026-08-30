// Package emit turns stage transitions into the two normalized signals:
// bounded metrics (sums and counts with the fixed ADR-0004 label set) and
// unsampled per-transaction outcome events. It also counts observed
// downstream provider calls, the one metric family that describes the
// provider rather than a transaction. It never touches a backend
// directly; exporters do.
//
// The types and interfaces here are the frozen v0.1.0 contract adapters
// implement; changes are interface amendments in their own reviewed PRs.
package emit

import (
	"context"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// The ADR-0005 age buckets, exported so no caller spells one by hand —
// a typo would mint a phantom series past the ADR-0004 cardinality fence.
const (
	AgeLt1m    = "lt1m"
	Age1mTo5m  = "1m-5m"
	Age5mTo30m = "5m-30m"
	Age30mTo2h = "30m-2h"
	AgeGt2h    = "gt2h"
)

// AgeBuckets lists the five buckets in ascending order.
var AgeBuckets = []string{AgeLt1m, Age1mTo5m, Age5mTo30m, Age30mTo2h, AgeGt2h}

// The provider-call outcomes (ADR-0004's `outcome` label on
// biz_provider_calls_total). This is the provider-health view and it is
// deliberately narrower than biz.Result: "success" means the provider
// returned a definitive answer — the API was reachable — and "failed"
// means it did not (transport error, timeout, 5xx, 429). A business
// decline is the provider answering correctly, so it is a success here
// and a failed *outcome event* on the stage, never both on this family.
// Exported so no caller spells one by hand.
const (
	ProviderCallSuccess = "success"
	ProviderCallFailed  = "failed"
)

// ProviderCallOutcomes lists the two accepted provider-call outcomes.
var ProviderCallOutcomes = []string{ProviderCallSuccess, ProviderCallFailed}

// ProviderOther is the fixed value a (provider, op) pair collapses to once
// an emitter has already minted DefaultProviderPairCap distinct pairs. The
// call is still counted — sums stay complete — but it cannot mint a new
// series. ADR-0004 calls provider and op "bounded by construction"; this
// is what makes that a library guarantee rather than a caller promise.
const ProviderOther = "other"

// MetricPoint is one bounded-label metric observation. Name is one of the
// fixed ADR-0004 families; Labels never exceed the family's declared set
// and never carry entity or customer ids — the emitter enforces that when
// constructing points (cardinality protection is a library guarantee, not
// exporter goodwill).
//
// For counter families Value is a delta — the increment observed at At,
// never a cumulative total; for gauge families it is the level observed
// at At. Batching exporters stamp backends with At, never with flush
// time — a batch delayed by an incident must not move money in time.
type MetricPoint struct {
	Name   string
	Labels map[string]string
	// Value is int64: counters count, and value sums are minor
	// units (ADR-0001).
	Value int64
	At    time.Time
}

// Caps declares what an exporter can honestly do. Honesty is
// load-bearing: a metrics-only exporter says Events=false and the engine
// reports the customers leg as unavailable rather than silently empty.
// query.Caps is the read-side mirror of this shape — amend both together
// (a parity test in each package pins the tie).
type Caps struct {
	Metrics            bool
	Events             bool
	MetricHistoryWeeks int
	EventHistoryWeeks  int
}

// Exporter ships the two signal kinds to a backend. Implementations live
// under adapters/ as nested modules and must pass the exporter
// conformance suite in testkit: batching, flush on Shutdown, capability
// honesty. Export failure semantics bind every implementation (ADR-0002):
// never block the caller, drop with a visible counter, never silently.
type Exporter interface {
	ExportMetrics(ctx context.Context, batch []MetricPoint) error
	ExportEvents(ctx context.Context, batch []biz.Outcome) error
	Capabilities() Caps
	Shutdown(ctx context.Context) error
}

// Option adjusts one Record call.
type Option func(*RecordConfig)

// RecordConfig is the mutable bag Options write into.
type RecordConfig struct {
	// Source overrides the outcome's Source field (e.g. "stripe:webhook").
	Source string
	// Err carries a short failure description onto the outcome.
	Err string
	// At overrides the outcome's event time. Webhook-fed adapters must
	// set it from the provider's event timestamp: deliveries arrive hours
	// late during exactly the outages this library measures, and
	// receipt-time stamping would move realized loss across incident
	// windows.
	At time.Time
}

// Emitter is the application-facing surface: one call per stage
// transition, one gauge update path for in-flight value, and one counter
// path for observed downstream provider calls.
//
// Record never blocks the request path and returns no error — invalid or
// overflowing outcomes are dropped and counted on
// biz_dropped_events_total, never silently. Outcome events are emitted
// regardless of trace sampling — money accounting never depends on a
// sampler.
type Emitter interface {
	Record(ctx context.Context, stage string, result biz.Result, opts ...Option)
	// SetInFlight publishes one sample of the in-flight family for a
	// (flow, stage, age_bucket, currency): biz_inflight_value (money) and
	// biz_inflight_count (count) together (ADR-0012).
	SetInFlight(flow, stage, ageBucket string, money biz.Money, count int64)
	// RecordProviderCall counts one observed downstream provider call on
	// biz_provider_calls_total{provider, op, outcome} (ADR-0004). It is
	// the write side of the counter engine.Compute reads to tell an
	// upstream provider failure from internal suppression; outcome must
	// be one of ProviderCallOutcomes, and provider/op are adapter-supplied
	// constants held inside a cardinality fence.
	RecordProviderCall(provider, op, outcome string)
}
