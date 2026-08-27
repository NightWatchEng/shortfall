// Package engine computes the impact report for a window and scope:
// realized, deferred, unrealized, customers, and coverage. It imports
// only query and registry (plus biz types) — the engine-import-boundary
// gate rule enforces exactly that, and the Querier's four verbs are the
// only questions it may ask a backend.
//
// FROZEN SURFACE (v0.1.0): the Request/Report shapes and the Compute
// signature. Leg computations land milestone by milestone; until a leg
// is implemented, Compute says so loudly rather than returning a
// plausible-looking zero.
package engine

import (
	"context"
	"errors"

	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// Scope narrows a report to part of the system, expressed as ADR-0004
// label filters (e.g. {"stage": "capture"}) — never backend-specific
// selectors.
type Scope map[string]string

// Request is the whole question: which window, which slice, which flows.
type Request struct {
	Window query.TimeRange
	Scope  Scope
	Flows  []string
}

// Evidence labels how a number is known. Realized and estimated value are
// NEVER merged into one headline figure — the label rides every leg so no
// renderer or consumer can blur them.
type Evidence string

const (
	EvidenceDeterministic Evidence = "deterministic"
	EvidenceEstimate      Evidence = "estimate"
	EvidenceTrust         Evidence = "trust"
)

// Leg is a deterministic money total: native per-currency sums (ADR-0001
// — no silent normalization), a transaction count, and its evidence label.
type Leg struct {
	Count      int64
	ByCurrency map[string]int64 // minor units per ISO 4217 code
	Evidence   Evidence
	Caveats    []string // e.g. "metrics-only: upper bound, not de-duped"
}

// DeferredLeg is in-flight value at the window's snapshot instant, with
// the age structure and SLA arithmetic the pager cares about.
type DeferredLeg struct {
	Leg
	ByAgeBucket        map[string]int64 // ADR-0005 buckets -> minor units
	SLABreaches        int64
	OldestAgeMinutes   int64
	ProjectedLostMinor map[string]int64 // per currency, per registry on_breach
}

// EstLeg is a counterfactual range: never a point, always [Low, High]
// around Mid, in minor units per currency.
type EstLeg struct {
	LowMinor  map[string]int64
	MidMinor  map[string]int64
	HighMinor map[string]int64
	Evidence  Evidence // always EvidenceEstimate; carried for renderers
	Notes     []string // recovery assumption, retention gaps, attribution hints
}

// CustomerImpact is one affected account for the top-N list.
type CustomerImpact struct {
	CustomerID string
	Segment    string
	ByCurrency map[string]int64
}

// CustomersLeg is who was hit. When the querier cannot serve events, the
// leg says WHY it is unavailable instead of rendering zeros.
type CustomersLeg struct {
	Distinct           int64
	BySegment          map[string]int64
	TopN               []CustomerImpact
	NotAvailableReason string
}

// CoverageLeg is the trust line: how much of the telemetry money sums
// reconciled against the ledger source, and when that was last measured.
type CoverageLeg struct {
	Ratio       float64
	Window      query.TimeRange
	Source      string
	Evidence    Evidence // always EvidenceTrust
	Unavailable string   // reason, when no reconciliation has run
}

// Report is the four legs plus trust and a severity suggestion.
type Report struct {
	Request    Request
	Realized   Leg
	Deferred   DeferredLeg
	Unrealized EstLeg
	Customers  CustomersLeg
	Coverage   CoverageLeg
	Severity   string
}

// ErrNotImplemented marks legs whose milestone has not landed. Compute
// returns it rather than fabricating a zero-filled Report — a
// plausible-looking empty report during an incident is worse than an
// honest error.
var ErrNotImplemented = errors.New("engine: not implemented yet (deterministic legs land in M6, counterfactual in M7)")

// Compute answers a Request against whatever backend the Querier fronts.
func Compute(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (Report, error) {
	_ = ctx
	_ = reg
	_ = q
	return Report{Request: req}, ErrNotImplemented
}
