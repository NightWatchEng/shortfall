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
	"time"

	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// Scope narrows a report to part of the system, expressed as ADR-0004
// label filters (e.g. {"stage": "capture"}) — never backend-specific
// selectors. DELIBERATE LIMIT, decided at the freeze: deployment axes
// (region, cluster, environment) are NOT biz.* labels and cannot be
// scoped here — a per-region deployment points Compute at that region's
// Querier instead. Widening the label universe is an ADR-0004 amendment,
// not a Scope key.
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
// the age structure and SLA arithmetic the pager cares about. Every
// money map is per-currency: minor units are not summable across
// currencies (ADR-0001), and the source gauge carries a currency label,
// so collapsing it here would destroy information the report cannot
// recover.
type DeferredLeg struct {
	Leg
	ByAgeBucket        map[string]map[string]int64 // ADR-0005 bucket -> currency -> minor units
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

// Report is the four legs plus trust, a severity suggestion, and the
// provenance a postmortem needs: when it was computed and against which
// co-signed registry — two reports for the same window that disagree
// must be explainable.
type Report struct {
	Request         Request
	GeneratedAt     time.Time
	RegistryVersion int
	LibraryVersion  string
	Realized        Leg
	Deferred        DeferredLeg
	Unrealized      EstLeg
	Customers       CustomersLeg
	Coverage        CoverageLeg
	Severity        string
}

// LibraryVersion identifies the engine's report contract for provenance.
const LibraryVersion = "v0.1.0"

// defaultTopN is how many accounts the customers leg lists when Compute
// assembles a report.
const defaultTopN = 10

// Compute assembles the impact report from the deterministic legs (realized,
// deferred, customers) against whatever backend the Querier fronts. A leg
// that its backend cannot ground is marked unavailable on that leg — a
// caveat for the money legs, a NotAvailableReason for customers — rather than
// failing the whole report or fabricating a zero. The counterfactual
// (unrealized, M7) and trust (coverage, M8) legs are not yet computed and say
// so explicitly.
func Compute(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (Report, error) {
	report := Report{
		Request:        req,
		GeneratedAt:    time.Now().UTC(),
		LibraryVersion: LibraryVersion,
	}
	if reg != nil {
		report.RegistryVersion = reg.Version
	}

	if leg, err := RealizedLeg(ctx, reg, q, req); err != nil {
		report.Realized = Leg{Evidence: EvidenceDeterministic, ByCurrency: map[string]int64{}, Caveats: []string{"unavailable: " + err.Error()}}
	} else {
		report.Realized = leg
	}

	if leg, err := Deferred(ctx, reg, q, req); err != nil {
		report.Deferred = DeferredLeg{Leg: Leg{Evidence: EvidenceDeterministic, ByCurrency: map[string]int64{}, Caveats: []string{"unavailable: " + err.Error()}}}
	} else {
		report.Deferred = leg
	}

	if leg, err := Customers(ctx, reg, q, req, defaultTopN); err != nil {
		report.Customers = CustomersLeg{NotAvailableReason: "query failed: " + err.Error()}
	} else {
		report.Customers = leg
	}

	// Not yet landed — stated honestly rather than rendered as zero.
	report.Unrealized = EstLeg{Evidence: EvidenceEstimate, Notes: []string{"counterfactual (unrealized) leg lands in M7 — not yet computed"}}
	report.Coverage = CoverageLeg{Evidence: EvidenceTrust, Unavailable: "reconciliation lands in M8 — no coverage ratio computed yet"}

	return report, nil
}
