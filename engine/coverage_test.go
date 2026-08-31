// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/NightWatchEng/shortfall/testkit"
)

// ledgerFromResult builds the provider-side success ledger from a harness run's
// ground truth: every settled transaction, summed to one row per currency.
func ledgerFromResult(res checkout.Result) []biz.LedgerRow {
	byCur := map[string]*biz.LedgerRow{}
	for _, tx := range res.Ledger.Txns {
		if tx.State != checkout.StateSettled {
			continue
		}

		r := byCur[tx.Currency]
		if r == nil {
			r = &biz.LedgerRow{Flow: "invoice.pay", Outcome: biz.ResultSuccess, Money: biz.Money{Currency: tx.Currency, Exponent: 2}}
			byCur[tx.Currency] = r
		}

		r.Money.Amount += tx.AmountMinor
		r.Count++
	}

	out := make([]biz.LedgerRow, 0, len(byCur))
	for _, r := range byCur {
		out = append(out, *r)
	}

	return out
}

// dropSettled returns a copy of res keeping only every keepEvery-th settled
// transaction (all non-settled kept), modelling a partial exporter drop.
func dropSettled(res checkout.Result, keepEvery int) checkout.Result {
	var kept []checkout.Txn
	n := 0
	for _, tx := range res.Ledger.Txns {
		if tx.State == checkout.StateSettled {
			n++
			if n%keepEvery != 0 {
				continue // dropped by the failing exporter
			}
		}

		kept = append(kept, tx)
	}

	return checkout.Result{Ledger: checkout.Ledger{Txns: kept}}
}

func coverageScenario(t *testing.T) checkout.Result {
	t.Helper()
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	return checkout.Run(checkout.Config{Seed: 11, Start: start, End: start.Add(3 * time.Hour)})
}

func coverageWindow() query.TimeRange {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	return query.TimeRange{From: start, To: start.Add(24 * time.Hour)}
}

func TestCoverageHarnessReports100(t *testing.T) {
	res := coverageScenario(t)
	ledger := ledgerFromResult(res)
	if len(ledger) == 0 {
		t.Fatal("scenario produced no settled transactions")
	}

	q := testkit.QuerierFromResult(res) // telemetry sees exactly what settled
	leg, slices, err := Coverage(
		context.Background(),
		nil,
		q,
		Request{Window: coverageWindow(), Flows: []string{"invoice.pay"}},
		ledger,
		"harness",
	)
	if err != nil {
		t.Fatal(err)
	}

	if leg.Unavailable != "" {
		t.Fatalf("coverage unexpectedly unavailable: %s", leg.Unavailable)
	}

	if leg.Ratio != 1.0 {
		t.Fatalf("full telemetry must reconcile to 100%%, got %.4f (slices %+v)", leg.Ratio, slices)
	}

	if leg.Evidence != EvidenceTrust {
		t.Fatalf("evidence = %q, want trust", leg.Evidence)
	}

	for _, s := range slices {
		if s.TelemetryMinor != s.LedgerMinor {
			t.Fatalf("slice %s/%s: telemetry %d != ledger %d at 100%%", s.Flow, s.Currency, s.TelemetryMinor, s.LedgerMinor)
		}
	}
}

func TestCoverageDroppedExporterUnder100WithDelta(t *testing.T) {
	res := coverageScenario(t)
	ledger := ledgerFromResult(res)
	// The exporter dropped every other settled txn from telemetry; the ledger is
	// still complete. Coverage must fall below 100% and the delta must be
	// attributed to the affected slice.
	q := testkit.QuerierFromResult(dropSettled(res, 2))
	leg, slices, err := Coverage(
		context.Background(),
		nil,
		q,
		Request{Window: coverageWindow(), Flows: []string{"invoice.pay"}},
		ledger,
		"harness",
	)
	if err != nil {
		t.Fatal(err)
	}

	if !(leg.Ratio > 0 && leg.Ratio < 1) {
		t.Fatalf("dropped-exporter coverage must be strictly between 0 and 1, got %.4f", leg.Ratio)
	}

	// Attribution: a slice must show telemetry below ledger by the dropped value.
	found := false
	for _, s := range slices {
		if s.TelemetryMinor < s.LedgerMinor {
			found = true
			if s.Ratio >= 1 {
				t.Fatalf("under-covered slice %s/%s reported ratio %.4f", s.Flow, s.Currency, s.Ratio)
			}
		}
	}

	if !found {
		t.Fatalf("dropped exporter must produce an under-covered slice: %+v", slices)
	}
}

func TestCoverageNoLedgerUnavailable(t *testing.T) {
	q := memq.New(memq.WithCaps(query.Caps{Metrics: true}))
	leg, slices, err := Coverage(context.Background(), nil, q, Request{Window: coverageWindow(), Flows: []string{"invoice.pay"}}, nil, "none")
	if err != nil {
		t.Fatal(err)
	}

	if leg.Unavailable == "" || leg.Ratio != 0 || slices != nil {
		t.Fatalf("no ledger must be Unavailable with no fabricated ratio, got %+v", leg)
	}
}

func TestCoverageOverTelemetryClampedToFull(t *testing.T) {
	// Telemetry exceeding the ledger (e.g. a double-counting exporter) is capped
	// at full coverage, never >100% — the ledger is the denominator (ADR-0011).
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	q := memq.New(memq.WithMetrics([]emit.MetricPoint{
		{Name: "biz_value_total", Value: 30000, At: window.From.Add(time.Minute), Labels: map[string]string{
			"flow": "invoice.pay", "stage": "settle", "outcome": "success", "currency": "USD", "kind": "fee", "segment": "smb"}},
	}), memq.WithCaps(query.Caps{Metrics: true}))
	ledger := []biz.LedgerRow{{
		Flow:    "invoice.pay",
		Outcome: biz.ResultSuccess,
		Money:   biz.Money{Amount: 10000, Currency: "USD", Exponent: 2},
		Count:   1,
	}}
	leg, slices, err := Coverage(context.Background(), nil, q, Request{Window: window, Flows: []string{"invoice.pay"}}, ledger, "test")
	if err != nil {
		t.Fatal(err)
	}

	if leg.Ratio != 1.0 || slices[0].Ratio != 1.0 {
		t.Fatalf("telemetry > ledger must clamp to 1.0, got headline %.4f slice %.4f", leg.Ratio, slices[0].Ratio)
	}
}

func TestCoverageZeroValueSliceSkipped(t *testing.T) {
	// A legitimate $0 success ledger slice (Count>0, Amount==0) must be skipped,
	// not scored 0 — otherwise it would tank the headline to 0%. A real USD slice
	// alongside it stays the headline.
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	q := memq.New(memq.WithMetrics([]emit.MetricPoint{
		{Name: "biz_value_total", Value: 10000, At: window.From.Add(time.Minute), Labels: map[string]string{
			"flow": "invoice.pay", "stage": "settle", "outcome": "success", "currency": "USD", "kind": "fee", "segment": "smb"}},
	}), memq.WithCaps(query.Caps{Metrics: true}))
	ledger := []biz.LedgerRow{
		{Flow: "invoice.pay", Outcome: biz.ResultSuccess, Money: biz.Money{Amount: 10000, Currency: "USD", Exponent: 2}, Count: 2},
		{Flow: "invoice.pay", Outcome: biz.ResultSuccess, Money: biz.Money{Amount: 0, Currency: "EUR", Exponent: 2}, Count: 3}, // $0 slice
	}
	leg, slices, err := Coverage(context.Background(), nil, q, Request{Window: window, Flows: []string{"invoice.pay"}}, ledger, "test")
	if err != nil {
		t.Fatal(err)
	}

	if leg.Ratio != 1.0 {
		t.Fatalf("a $0 slice must be skipped, not tank the headline; got %.4f (slices %+v)", leg.Ratio, slices)
	}

	for _, s := range slices {
		if s.Currency == "EUR" {
			t.Fatalf("zero-value EUR slice must be skipped, not emitted: %+v", s)
		}
	}
}

func TestCoverageAllZeroLedgerUnavailable(t *testing.T) {
	// A ledger of only zero-value success rows has no value to reconcile: the
	// leg is Unavailable, not a fabricated 100% (Ratio must stay 0, unset).
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	q := memq.New(memq.WithCaps(query.Caps{Metrics: true}))
	ledger := []biz.LedgerRow{{
		Flow:    "invoice.pay",
		Outcome: biz.ResultSuccess,
		Money:   biz.Money{Amount: 0, Currency: "USD", Exponent: 2},
		Count:   4,
	}}
	leg, slices, err := Coverage(context.Background(), nil, q, Request{Window: window, Flows: []string{"invoice.pay"}}, ledger, "test")
	if err != nil {
		t.Fatal(err)
	}

	if leg.Unavailable == "" || leg.Ratio != 0 || len(slices) != 0 {
		t.Fatalf("all-zero ledger must be Unavailable with no ratio/slices, got %+v (slices %+v)", leg, slices)
	}

	if !strings.Contains(leg.Unavailable, "value") {
		t.Fatalf("message should name the zero-value cause, got %q", leg.Unavailable)
	}
}

func TestCoverageWorstSliceIsHeadline(t *testing.T) {
	// Two currencies: USD fully covered, EUR half covered. The headline is the
	// worst slice (EUR ~0.5), not an average (ADR-0011).
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	at := window.From.Add(time.Minute)
	val := func(cur string, v int64) emit.MetricPoint {
		return emit.MetricPoint{Name: "biz_value_total", Value: v, At: at, Labels: map[string]string{
			"flow": "invoice.pay", "stage": "settle", "outcome": "success", "currency": cur, "kind": "fee", "segment": "smb"}}
	}
	q := memq.New(memq.WithMetrics([]emit.MetricPoint{val("USD", 10000), val("EUR", 5000)}), memq.WithCaps(query.Caps{Metrics: true}))
	ledger := []biz.LedgerRow{
		{Flow: "invoice.pay", Outcome: biz.ResultSuccess, Money: biz.Money{Amount: 10000, Currency: "USD", Exponent: 2}, Count: 5},
		{Flow: "invoice.pay", Outcome: biz.ResultSuccess, Money: biz.Money{Amount: 10000, Currency: "EUR", Exponent: 2}, Count: 5},
	}
	leg, slices, err := Coverage(context.Background(), nil, q, Request{Window: window, Flows: []string{"invoice.pay"}}, ledger, "test")
	if err != nil {
		t.Fatal(err)
	}

	if leg.Ratio != 0.5 {
		t.Fatalf("headline must be the worst slice (EUR 0.5), got %.4f (slices %+v)", leg.Ratio, slices)
	}
}

// perStagePoint builds one success biz_value_total point at a stage — the
// shape the real emitter ships at every successful stage transition.
func perStagePoint(stage, currency string, v int64, at time.Time) emit.MetricPoint {
	return emit.MetricPoint{Name: "biz_value_total", Value: v, At: at, Labels: map[string]string{
		"flow": "invoice.pay", "stage": stage, "outcome": "success", "currency": currency, "kind": "fee", "segment": "smb"}}
}

// TestCoverageValueStageAnchored pins the stage-anchored telemetry read: the
// real emitter ships success value at every stage transition, so an
// unfiltered cross-stage sum multiply-counts and a clamped ratio can mask an
// exporter drop entirely. Telemetry is read at the flow's value stage — the
// declared reconcile stage, defaulting to the last stage.
func TestCoverageValueStageAnchored(t *testing.T) {
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	at := window.From.Add(time.Minute)
	regYAML := func(reconcile string) string {
		return `
version: 1
segments: [smb]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
      - { name: settle,  signals: ["queue:settle.q"] }
    sla:
      capture: { deadline: PT30M, on_breach: lost }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 4 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.0 }
    reconcile: { ` + reconcile + ` }
`
	}
	// Two settled txns of 100 each; the ledger books 200.
	full := []emit.MetricPoint{
		perStagePoint("auth", "USD", 100, at), perStagePoint("capture", "USD", 100, at), perStagePoint("settle", "USD", 100, at),
		perStagePoint("auth", "USD", 100, at), perStagePoint("capture", "USD", 100, at), perStagePoint("settle", "USD", 100, at),
	}
	ledger := []biz.LedgerRow{{
		Flow: "invoice.pay", Outcome: biz.ResultSuccess,
		Money: biz.Money{Amount: 200, Currency: "USD", Exponent: 2}, Count: 2,
	}}
	cases := []struct {
		name      string
		reconcile string
		points    []emit.MetricPoint
		wantRatio float64
	}{
		{
			name:      "full per-stage telemetry reads once per txn, full coverage",
			reconcile: `source: "sql:ledger.payments"`,
			points:    full,
			wantRatio: 1.0,
		},
		{
			// The masked-drop defect: the exporter lost one whole txn (all
			// three of its stage points). The cross-stage sum still reads
			// 300 >= 200 and clamps to full; the settle-anchored read sees
			// 100/200.
			name:      "a dropped txn shows as half coverage, not clamped full",
			reconcile: `source: "sql:ledger.payments"`,
			points:    full[:3],
			wantRatio: 0.5,
		},
		{
			// A declared reconcile stage anchors the read there: with settle
			// points missing entirely (a settle-blind exporter), capture
			// still carries every txn once.
			name:      "declared reconcile stage wins over the last-stage default",
			reconcile: `source: "sql:ledger.payments", stage: capture`,
			points: []emit.MetricPoint{
				perStagePoint("auth", "USD", 100, at), perStagePoint("capture", "USD", 100, at),
				perStagePoint("auth", "USD", 100, at), perStagePoint("capture", "USD", 100, at),
			},
			wantRatio: 1.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.yaml")
			if err := os.WriteFile(path, []byte(regYAML(c.reconcile)), 0o600); err != nil {
				t.Fatal(err)
			}

			reg, err := registry.Load(path)
			if err != nil {
				t.Fatal(err)
			}

			q := memq.New(memq.WithMetrics(c.points), memq.WithCaps(query.Caps{Metrics: true}))
			leg, _, err := Coverage(context.Background(), &reg, q, Request{Window: window, Flows: []string{"invoice.pay"}}, ledger, "test")
			if err != nil {
				t.Fatal(err)
			}

			if leg.Ratio != c.wantRatio {
				t.Fatalf("ratio = %.4f, want %.4f", leg.Ratio, c.wantRatio)
			}
		})
	}
}

// TestCoverageUnknownFlowFailsLoudly pins the fail-loud contract: a ledger
// flow the registry does not know errors rather than reading unanchored — a
// silently unfiltered ratio could clamp-mask exporter drops (ADR-0016).
func TestCoverageUnknownFlowFailsLoudly(t *testing.T) {
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}

	q := memq.New(memq.WithMetrics(nil), memq.WithCaps(query.Caps{Metrics: true}))
	ledger := []biz.LedgerRow{{
		Flow: "invoice.pya", Outcome: biz.ResultSuccess, // ledger/registry name drift
		Money: biz.Money{Amount: 100, Currency: "USD", Exponent: 2}, Count: 1,
	}}
	_, _, err = Coverage(context.Background(), &reg, q, Request{Window: window}, ledger, "test")
	if err == nil || !strings.Contains(err.Error(), "invoice.pya") {
		t.Fatalf("unknown ledger flow must fail loudly naming the flow, got %v", err)
	}
}
