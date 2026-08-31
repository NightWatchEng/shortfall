// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
)

func unrealizedRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}

	return &reg
}

// txnPoint / valuePoint build the metric points the emitter would record.
func txnPoint(stage, outcome string, at time.Time, v int64) emit.MetricPoint {
	return emit.MetricPoint{Name: "biz_txn_total", Value: v, At: at, Labels: map[string]string{
		"flow": "invoice.pay", "stage": stage, "outcome": outcome, "currency": "USD", "segment": "smb"}}
}
func valuePoint(stage, outcome string, at time.Time, v int64) emit.MetricPoint {
	return emit.MetricPoint{Name: "biz_value_total", Value: v, At: at, Labels: map[string]string{
		"flow": "invoice.pay", "stage": stage, "outcome": outcome, "currency": "USD", "kind": "fee", "segment": "smb"}}
}

func TestUnrealizedSuppressionWithinInterval(t *testing.T) {
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) // Monday
	const hour = 10 * time.Hour
	// Eight prior Mondays at 10:00, entries varying around 100 (median 100, MAD>0
	// so the baseline reports a real range).
	histCounts := []int64{80, 90, 95, 100, 100, 105, 110, 120}
	var pts []emit.MetricPoint
	for w, c := range histCounts {
		at := baseMon.Add(time.Duration(w)*7*24*time.Hour + hour)
		pts = append(pts, txnPoint("auth", "success", at, c)) // stage-entry count
	}

	// Incident hour: the 9th Monday 10:00. Only 40 entries observed (60
	// suppressed). The AOV ratio reads at the flow's value stage (settle),
	// where the paired count/value give AOV = 200000/40 = 5000.
	incident := baseMon.Add(8*7*24*time.Hour + hour)
	pts = append(pts,
		txnPoint("auth", "success", incident, 40),
		txnPoint("settle", "success", incident, 40),
		valuePoint("settle", "success", incident, 200000),
	)
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true}))

	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	// Exact bounds, pinned so a broken Low/High cannot pass. median 100, MAD 7.5,
	// band = 2*1.4826*7.5; the incident hour observed 40 at AOV 5000, recovery .6.
	band := 2 * 1.4826 * 7.5
	const (
		observed = 40.0
		aov      = 5000.0
		keep     = 1 - 0.6
	)
	wantMid := int64(math.Round((100 - observed) * aov * keep))
	wantLow := int64(math.Round((100 - band - observed) * aov * keep))
	wantHigh := int64(math.Round((100 + band - observed) * aov * keep))
	if leg.MidMinor["USD"] != wantMid || leg.LowMinor["USD"] != wantLow || leg.HighMinor["USD"] != wantHigh {
		t.Fatalf("range = [%d, %d] mid %d, want [%d, %d] mid %d",
			leg.LowMinor["USD"], leg.HighMinor["USD"], leg.MidMinor["USD"], wantLow, wantHigh, wantMid)
	}

	// Ground truth (60 suppressed × 5000 × 0.4 = 120000) is the Mid and lies
	// strictly inside the range, which is non-degenerate (ADR-0006).
	if wantMid != 120000 || leg.LowMinor["USD"] >= leg.HighMinor["USD"] {
		t.Fatalf("expected a real range around a 120000 mid, got [%d,%d] mid %d", leg.LowMinor["USD"], leg.HighMinor["USD"], leg.MidMinor["USD"])
	}

	if leg.Evidence != EvidenceEstimate {
		t.Fatalf("evidence = %q, want estimate", leg.Evidence)
	}

	// Recovery is disclosed.
	if !hasNoteContaining(leg.Notes, "recovery") {
		t.Fatalf("recovery assumption not disclosed: %v", leg.Notes)
	}
}

func TestUnrealizedNoSuppressionIsZero(t *testing.T) {
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		at := baseMon.Add(time.Duration(w)*7*24*time.Hour + hour)
		pts = append(pts, txnPoint("auth", "success", at, 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	// Observed entries MEET expectation (100) — no suppression, no unrealized loss.
	pts = append(pts, txnPoint("auth", "success", incident, 100),
		valuePoint("auth", "success", incident, 500000))
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true}))
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	if leg.MidMinor["USD"] != 0 {
		t.Fatalf("no suppression must yield 0 mid, got %d", leg.MidMinor["USD"])
	}
}

func TestUnrealizedUpstreamAttribution(t *testing.T) {
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		pts = append(pts, txnPoint("auth", "success", baseMon.Add(time.Duration(w)*7*24*time.Hour+hour), 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	pts = append(pts, txnPoint("auth", "success", incident, 40),
		valuePoint("auth", "success", incident, 200000),
		// The wrapped client recorded failed provider calls during the window.
		emit.MetricPoint{Name: "biz_provider_calls_total", Value: 12, At: incident,
			Labels: map[string]string{"provider": "stripe", "op": "payment_intents.create", "outcome": "failed"}})
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true}))
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	if !hasNoteContaining(leg.Notes, "upstream") {
		t.Fatalf("failed provider calls should produce an upstream attribution hint: %v", leg.Notes)
	}
}

func TestUnrealizedNoRegistryUnavailable(t *testing.T) {
	q := memq.New(memq.WithCaps(query.Caps{Metrics: true}))
	req := Request{Window: query.TimeRange{From: time.Unix(0, 0), To: time.Unix(3600, 0)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), nil, q, req)
	if err != nil {
		t.Fatal(err)
	}

	if len(leg.MidMinor) != 0 || !hasNoteContaining(leg.Notes, "registry") {
		t.Fatalf("no registry must be unavailable with a note, got %+v", leg)
	}
}

func TestUnrealizedNoMetricsUnavailable(t *testing.T) {
	reg := unrealizedRegistry(t)
	q := memq.New(memq.WithCaps(query.Caps{Events: true})) // events-only backend
	req := Request{Window: query.TimeRange{From: time.Unix(0, 0), To: time.Unix(3600, 0)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	if !hasNoteContaining(leg.Notes, "metric source") {
		t.Fatalf("a metrics-less backend must be unavailable with a note, got %v", leg.Notes)
	}
}

func TestUnrealizedNonHourAlignedWindow(t *testing.T) {
	// A responder-supplied window need not start on the hour. Observed entries
	// must still pair with the right target hour: with a 10:30 start the single
	// target hour is 11:00, and the 40 entries observed at 11:00 must be used —
	// not misbucketed to 0 (which would make the shortfall the full
	// expectation).
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	eleven := 11 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		pts = append(pts, txnPoint("auth", "success", baseMon.Add(time.Duration(w)*7*24*time.Hour+eleven), 100))
	}

	incidentHour := baseMon.Add(8*7*24*time.Hour + eleven) // Monday 11:00
	pts = append(pts,
		txnPoint("auth", "success", incidentHour, 40),
		txnPoint("settle", "success", incidentHour, 40),
		valuePoint("settle", "success", incidentHour, 200000),
	)
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true}))

	// Window 10:30 → 11:30: only the 11:00 hour begins inside it.
	req := Request{
		Window: query.TimeRange{From: incidentHour.Add(-30 * time.Minute), To: incidentHour.Add(30 * time.Minute)},
		Flows:  []string{"invoice.pay"},
	}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	// observed 40 (paired at 11:00), so mid = (100-40)*5000*0.4 = 120000.
	// A misalignment would read observed 0 → mid 200000.
	if leg.MidMinor["USD"] != 120000 {
		t.Fatalf("mid = %d, want 120000 (observed correctly paired at 11:00)", leg.MidMinor["USD"])
	}
}

func TestUnrealizedMultiCurrency(t *testing.T) {
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	pt := func(cur string, at time.Time, v int64) emit.MetricPoint {
		return emit.MetricPoint{Name: "biz_txn_total", Value: v, At: at, Labels: map[string]string{
			"flow": "invoice.pay", "stage": "auth", "outcome": "success", "currency": cur, "segment": "smb"}}
	}
	val := func(cur string, at time.Time, v int64) emit.MetricPoint {
		return emit.MetricPoint{Name: "biz_value_total", Value: v, At: at, Labels: map[string]string{
			"flow": "invoice.pay", "stage": "auth", "outcome": "success", "currency": cur, "kind": "fee", "segment": "smb"}}
	}
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		at := baseMon.Add(time.Duration(w)*7*24*time.Hour + hour)
		pts = append(pts, pt("USD", at, 100), pt("EUR", at, 50))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	// USD: 40 observed, AOV 5000 -> shortfall 60. EUR: 20 observed, AOV 3000 ->
	// shortfall 30. The AOV pairs read at the value stage (settle).
	settle := func(cur string, at time.Time, v int64) emit.MetricPoint {
		p := pt(cur, at, v)
		p.Labels["stage"] = "settle"
		return p
	}
	settleVal := func(cur string, at time.Time, v int64) emit.MetricPoint {
		p := val(cur, at, v)
		p.Labels["stage"] = "settle"
		return p
	}
	pts = append(pts, pt("USD", incident, 40), settle("USD", incident, 40), settleVal("USD", incident, 200000),
		pt("EUR", incident, 20), settle("EUR", incident, 20), settleVal("EUR", incident, 60000))
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true}))
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	// Per currency, never mixed. USD mid = 60*5000*0.4 = 120000; EUR mid = 30*3000*0.4 = 36000.
	if leg.MidMinor["USD"] != 120000 {
		t.Fatalf("USD mid = %d, want 120000", leg.MidMinor["USD"])
	}

	if leg.MidMinor["EUR"] != 36000 {
		t.Fatalf("EUR mid = %d, want 36000", leg.MidMinor["EUR"])
	}
}

func TestUnrealizedEstimatorFallbackAOV(t *testing.T) {
	// No observed captured successes in the window -> AOV falls back to the
	// registry estimator (default_minor 18750 for invoice.pay). Entries still
	// dropped (failed outcome), so a shortfall is valued at the estimator.
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		pts = append(pts, txnPoint("auth", "failed", baseMon.Add(time.Duration(w)*7*24*time.Hour+hour), 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	pts = append(pts, txnPoint("auth", "failed", incident, 40)) // 40 entered, all failed; no captured value
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true}))
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	// mid = (100-40) * 18750 estimator * 0.4 = 60*18750*0.4 = 450000.
	if leg.MidMinor["USD"] != 450000 {
		t.Fatalf("estimator-fallback mid = %d, want 450000", leg.MidMinor["USD"])
	}
}

func TestUnrealizedAOVFromEventsIncludesEstimated(t *testing.T) {
	// With an event source, AOV comes from success EVENTS, which carry estimated
	// amounts the biz_value_total counter omits — so the AOV (and the leg) are
	// not understated. Entries come from metrics; AOV from events.
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		pts = append(pts, txnPoint("auth", "success", baseMon.Add(time.Duration(w)*7*24*time.Hour+hour), 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	pts = append(pts, txnPoint("auth", "success", incident, 40))
	// Success events during the window: two real (4000, 6000) and one estimated
	// (10000) — mean 6666.67 -> AOV 6667. The value counter (which omits the
	// estimated one) would give (4000+6000)/3 = 3333, understated.
	ev := func(amt int64, estimated bool) biz.Outcome {
		return biz.Outcome{At: incident.Add(time.Minute), Stage: "settle", Result: biz.ResultSuccess,
			VC: biz.ValueContext{Flow: "invoice.pay", CustomerID: "h:c", Segment: "smb", Estimated: estimated,
				Money: biz.Money{Amount: amt, Currency: "USD", Exponent: 2}}}
	}
	q := memq.New(memq.WithMetrics(pts), memq.WithEvents([]biz.Outcome{ev(4000, false), ev(6000, false), ev(10000, true)}),
		memq.WithCaps(query.Caps{Metrics: true, Events: true}))
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	// mid = (100-40) * 6667 * 0.4 = 60 * 6667 * 0.4 = 160008.
	wantAOV := int64(math.Round((4000.0 + 6000 + 10000) / 3))
	wantMid := int64(math.Round(60 * float64(wantAOV) * 0.4))
	if leg.MidMinor["USD"] != wantMid {
		t.Fatalf("events-AOV mid = %d, want %d (AOV %d includes the estimated success)", leg.MidMinor["USD"], wantMid, wantAOV)
	}
}

func TestUnrealizedRetentionGap(t *testing.T) {
	// The querier serves only 4 weeks of history but invoice.pay's baseline needs
	// 8. The report must show the gap and a warehouse suggestion, and must not
	// silently compute a degraded baseline.
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ { // history is present, but the querier only PROMISES 4 weeks
		pts = append(pts, txnPoint("auth", "success", baseMon.Add(time.Duration(w)*7*24*time.Hour+hour), 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	// Entries are observed at the entry stage; the AOV ratio reads at the
	// flow's value stage (settle), so the value/count pair lives there.
	pts = append(pts,
		txnPoint("auth", "success", incident, 40),
		txnPoint("settle", "success", incident, 40),
		valuePoint("settle", "success", incident, 200000),
	)
	q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true, MetricHistoryWeeks: 4}))
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	if !hasNoteContaining(leg.Notes, "RETENTION GAP") || !hasNoteContaining(leg.Notes, "warehouse") {
		t.Fatalf("retention gap + suggestion must be shown, notes: %v", leg.Notes)
	}

	if len(leg.MidMinor) != 0 {
		t.Fatalf("no silent degraded baseline: expected no valued currencies, got %+v", leg.MidMinor)
	}
}

func TestUnrealizedRetentionNotAGap(t *testing.T) {
	// No gap when the querier serves at least the lookback: 0 = undeclared (not
	// "zero"), 8 = exactly the lookback (boundary — must stay no-gap, guarding
	// the strict `<`), 12 = more than enough. All proceed with the estimate.
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		pts = append(pts, txnPoint("auth", "success", baseMon.Add(time.Duration(w)*7*24*time.Hour+hour), 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	// Entries are observed at the entry stage; the AOV ratio reads at the
	// flow's value stage (settle), so the value/count pair lives there.
	pts = append(pts,
		txnPoint("auth", "success", incident, 40),
		txnPoint("settle", "success", incident, 40),
		valuePoint("settle", "success", incident, 200000),
	)
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}

	for _, hw := range []int{0, 8, 12} { // 0 undeclared, 8 == lookback, 12 > lookback
		t.Run(fmt.Sprintf("history_weeks_%d", hw), func(t *testing.T) {
			q := memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true, MetricHistoryWeeks: hw}))
			leg, err := Unrealized(context.Background(), reg, q, req)
			if err != nil {
				t.Fatal(err)
			}

			if hasNoteContaining(leg.Notes, "RETENTION GAP") {
				t.Fatalf("history_weeks=%d must not be a gap: %v", hw, leg.Notes)
			}

			if leg.MidMinor["USD"] != 120000 {
				t.Fatalf("history_weeks=%d: estimate must proceed, mid = %d want 120000", hw, leg.MidMinor["USD"])
			}
		})
	}
}

// eventsFailQuerier serves metrics from an inner memq but fails every event
// query, to exercise the AOV events-error disclosure path.
type eventsFailQuerier struct{ inner *memq.Querier }

func (e eventsFailQuerier) QueryMetric(ctx context.Context, q query.Query) (query.Series, error) {
	return e.inner.QueryMetric(ctx, q)
}
func (e eventsFailQuerier) QueryEvents(context.Context, query.EventQuery) (query.EventGroups, error) {
	return nil, errEventsDown
}
func (e eventsFailQuerier) Capabilities() query.Caps { return query.Caps{Metrics: true, Events: true} }

var errEventsDown = errors.New("events backend down")

func TestUnrealizedDisclosesEventsFailure(t *testing.T) {
	reg := unrealizedRegistry(t)
	baseMon := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const hour = 10 * time.Hour
	var pts []emit.MetricPoint
	for w := 0; w < 8; w++ {
		pts = append(pts, txnPoint("auth", "success", baseMon.Add(time.Duration(w)*7*24*time.Hour+hour), 100))
	}

	incident := baseMon.Add(8*7*24*time.Hour + hour)
	// Entries are observed at the entry stage; the AOV ratio reads at the
	// flow's value stage (settle), so the value/count pair lives there.
	pts = append(pts,
		txnPoint("auth", "success", incident, 40),
		txnPoint("settle", "success", incident, 40),
		valuePoint("settle", "success", incident, 200000),
	)
	q := eventsFailQuerier{inner: memq.New(memq.WithMetrics(pts), memq.WithCaps(query.Caps{Metrics: true, Events: true}))}
	req := Request{Window: query.TimeRange{From: incident, To: incident.Add(time.Hour)}, Flows: []string{"invoice.pay"}}
	leg, err := Unrealized(context.Background(), reg, q, req)
	if err != nil {
		t.Fatal(err)
	}

	// It must not silently succeed as if events were absent: the failure is
	// disclosed, and it still values via the counter fallback (mid 120000).
	if !hasNoteContaining(leg.Notes, "events query failed") {
		t.Fatalf("events-backend failure must be disclosed, notes: %v", leg.Notes)
	}

	if leg.MidMinor["USD"] != 120000 {
		t.Fatalf("counter fallback mid = %d, want 120000", leg.MidMinor["USD"])
	}
}

func hasNoteContaining(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}

	return false
}

// TestAOVMinorStageFiltered pins the AOV metric fallback to the flow's value
// stage: entry-stage counts (biz_txn_total at the entry stage with no
// companion value point) must not inflate the denominator and silently
// halve the AOV.
func TestAOVMinorStageFiltered(t *testing.T) {
	window := query.TimeRange{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()}
	at := window.From.Add(time.Minute)
	txn := func(stage string) emit.MetricPoint {
		return emit.MetricPoint{Name: "biz_txn_total", Value: 1, At: at, Labels: map[string]string{
			"flow": "invoice.pay", "stage": stage, "outcome": "success", "currency": "USD", "segment": "smb"}}
	}
	val := func(stage string, v int64) emit.MetricPoint {
		return emit.MetricPoint{Name: "biz_value_total", Value: v, At: at, Labels: map[string]string{
			"flow": "invoice.pay", "stage": stage, "outcome": "success", "currency": "USD", "kind": "fee", "segment": "smb"}}
	}
	// Four entries, two of which settled at 100 each: true AOV 100. The
	// stage-unfiltered ratio would read 200/(4+2) = 33.
	q := memq.New(memq.WithMetrics([]emit.MetricPoint{
		txn("auth"), txn("auth"), txn("auth"), txn("auth"),
		txn("settle"), val("settle", 100),
		txn("settle"), val("settle", 100),
	}), memq.WithCaps(query.Caps{Metrics: true}))
	f := registry.Flow{
		Name: "invoice.pay",
		Stages: []registry.Stage{
			{Name: "auth"}, {Name: "capture"}, {Name: "settle"},
		},
	}
	aov, source, warn, ok := aovMinor(context.Background(), q, "invoice.pay", "USD", f, window)
	if !ok || source != "metric" {
		t.Fatalf("aov unavailable or wrong source (ok=%v source=%q warn=%q)", ok, source, warn)
	}

	if aov != 100 {
		t.Fatalf("aov = %d, want 100 (value-stage anchored, entries excluded)", aov)
	}
}

func TestClampFractionNonFinite(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"nan maps to zero recovery", math.NaN(), 0},
		{"negative infinity", math.Inf(-1), 0},
		{"positive infinity", math.Inf(1), 1},
		{"negative", -0.25, 0},
		{"above one", 1.5, 1},
		{"in range", 0.4, 0.4},
		{"zero", 0, 0},
		{"one", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampFraction(tc.in)
			if got != tc.want {
				t.Fatalf("clampFraction(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
