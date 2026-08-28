package engine

import (
	"context"
	"fmt"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// CoverageSlice is one (flow, currency) reconciliation: what telemetry recorded
// as captured value vs what the ledger (the provider's books) recorded, and the
// resulting slice coverage ratio. It is the per-slice attribution the reconcile
// job prints so a sub-100% headline names exactly where telemetry and the
// ledger diverge.
type CoverageSlice struct {
	Flow           string
	Currency       string
	Exponent       int8 // the currency's minor-unit exponent, from the ledger row
	TelemetryMinor int64
	LedgerMinor    int64
	Ratio          float64 // TelemetryMinor / LedgerMinor, clamped [0,1]
}

// Coverage computes the trust line (ADR-0011): how much of the ledger's
// captured value telemetry also saw, per (flow, currency), reported as the
// WORST slice ratio. ledger carries the provider-side success rows (from the
// Stripe reconciler or a SQL ledger); source names it for the report. The
// per-slice breakdown is returned alongside for attribution.
//
// A slice in the ledger but unseen by telemetry is 0 coverage (the dropped-
// exporter case). A slice telemetry saw but the ledger did not does not lower
// the ratio (the ledger is the denominator of record) and is out of scope for
// v0 attribution — detecting a telemetry-side over-count is future work. A
// ledger slice that sums to zero value is skipped (coverage of zero is
// undefined, and a $0 slice must not tank the headline). With no success ledger
// rows — or none with value — the leg is Unavailable, never a fabricated 100%.
func Coverage(ctx context.Context, reg *registry.Registry, q query.Querier, req Request, ledger []biz.LedgerRow, source string) (CoverageLeg, []CoverageSlice, error) {
	_ = reg // reserved: per-flow currency/estimator validation lands with M8 polish
	leg := CoverageLeg{Window: req.Window, Source: source, Evidence: EvidenceTrust}

	// Ledger success value per (flow, currency), keeping the currency's exponent.
	type acc struct {
		minor    int64
		exponent int8
	}
	ledgerVal := map[string]map[string]*acc{}
	for _, r := range ledger {
		if r.Outcome != biz.ResultSuccess {
			continue
		}
		if ledgerVal[r.Flow] == nil {
			ledgerVal[r.Flow] = map[string]*acc{}
		}
		a := ledgerVal[r.Flow][r.Money.Currency]
		if a == nil {
			a = &acc{exponent: r.Money.Exponent}
			ledgerVal[r.Flow][r.Money.Currency] = a
		}
		a.minor += r.Money.Amount
	}
	if len(ledgerVal) == 0 {
		leg.Unavailable = "no ledger success rows to reconcile against"
		return leg, nil, nil
	}

	// Telemetry captured value per (flow, currency), for the flows the ledger
	// covers (or the requested flows, intersected with the ledger).
	flows := req.Flows
	if len(flows) == 0 {
		for f := range ledgerVal {
			flows = append(flows, f)
		}
	}

	var slices []CoverageSlice
	worst := 1.0
	haveSlice := false
	for _, flow := range flows {
		lcur := ledgerVal[flow]
		if lcur == nil {
			continue // requested flow not in the ledger — nothing to reconcile
		}
		tel, err := telemetrySuccessValue(ctx, q, flow, req.Window)
		if err != nil {
			return CoverageLeg{}, nil, fmt.Errorf("engine: coverage telemetry query for %q: %w", flow, err)
		}
		for currency, a := range lcur {
			if a.minor <= 0 {
				continue // no ledger value to reconcile in this slice — coverage is undefined, not 0; skipping keeps a $0 slice from tanking the trust number
			}
			telMinor := tel[currency]
			ratio := float64(telMinor) / float64(a.minor)
			if ratio > 1 {
				ratio = 1 // the ledger is the denominator; telemetry seeing more is full coverage, not >100%
			}
			slices = append(slices, CoverageSlice{Flow: flow, Currency: currency, Exponent: a.exponent, TelemetryMinor: telMinor, LedgerMinor: a.minor, Ratio: ratio})
			if ratio < worst {
				worst = ratio
			}
			haveSlice = true
		}
	}
	if !haveSlice {
		// Either no requested flow was in the ledger, or every matching slice
		// summed to zero value — in both cases there is nothing with value to
		// reconcile. Say so accurately rather than guessing which.
		leg.Unavailable = "no ledger slice with value to reconcile for the requested flows"
		return leg, nil, nil
	}
	leg.Ratio = worst
	return leg, slices, nil
}

// telemetrySuccessValue returns captured (success) value per currency for a
// flow over the window: the biz_value_total metric when the backend serves it,
// otherwise the exact sum of success outcome events.
func telemetrySuccessValue(ctx context.Context, q query.Querier, flow string, window query.TimeRange) (map[string]int64, error) {
	caps := q.Capabilities()
	out := map[string]int64{}
	switch {
	case caps.Metrics:
		series, err := q.QueryMetric(ctx, query.Query{
			Metric:  "biz_value_total",
			Agg:     query.AggSum,
			Filters: map[string]string{"flow": flow, "outcome": "success"},
			GroupBy: []string{"currency"},
			Range:   window,
		})
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			for _, p := range s.Points {
				out[s.Labels["currency"]] += int64(p.Value)
			}
		}
	case caps.Events:
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   window,
			Filters: map[string]string{"flow": flow, "outcome": "success"},
			GroupBy: []string{"currency"},
		})
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			out[g.Key["currency"]] += g.SumMinor
		}
	default:
		return nil, query.ErrUnsupported
	}
	return out, nil
}
