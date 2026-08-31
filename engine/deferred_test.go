// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/NightWatchEng/shortfall/testkit"
)

func inflightPoint(stage, bucket, currency string, value int64, at time.Time) emit.MetricPoint {
	return emit.MetricPoint{
		Name:   "biz_inflight_value",
		Labels: map[string]string{"flow": "invoice.pay", "stage": stage, "age_bucket": bucket, "currency": currency},
		Value:  value, At: at,
	}
}

// testRegistry loads the reference registry (capture SLA PT30M -> lost,
// settle SLA P1D -> at_risk).
func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}

	return &reg
}

func TestDeferredByBucketAndCurrency(t *testing.T) {
	at := win.To.Add(-time.Minute)
	metrics := []emit.MetricPoint{
		inflightPoint("capture", "5m-30m", "USD", 1000, at),
		inflightPoint("capture", "gt2h", "USD", 500, at),   // breached (>= 30m) -> lost
		inflightPoint("capture", "30m-2h", "EUR", 700, at), // breached -> lost, EUR
		inflightPoint("settle", "gt2h", "USD", 9000, at),   // settle SLA P1D not breached at gt2h(120m)
	}
	q := memq.New(memq.WithMetrics(metrics))
	leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}

	if leg.ByCurrency["USD"] != 10500 || leg.ByCurrency["EUR"] != 700 {
		t.Fatalf("ByCurrency = %v, want USD 10500 EUR 700", leg.ByCurrency)
	}

	if leg.ByAgeBucket["5m-30m"]["USD"] != 1000 || leg.ByAgeBucket["gt2h"]["USD"] != 9500 {
		t.Fatalf("ByAgeBucket = %v", leg.ByAgeBucket)
	}

	// Projected-lost: capture breaches (30m boundary) where on_breach=lost.
	// USD gt2h 500 + EUR 30m-2h 700; settle is at_risk (not lost) and the
	// capture 5m-30m is not past the 30m deadline.
	if leg.ProjectedLostMinor["USD"] != 500 || leg.ProjectedLostMinor["EUR"] != 700 {
		t.Fatalf("ProjectedLostMinor = %v, want USD 500 EUR 700", leg.ProjectedLostMinor)
	}

	if leg.OldestAgeMinutes != 120 {
		t.Fatalf("OldestAgeMinutes = %d, want 120 (gt2h floor)", leg.OldestAgeMinutes)
	}

	if leg.Evidence != EvidenceDeterministic {
		t.Fatalf("evidence = %q", leg.Evidence)
	}

	// Counts are the honest gap.
	if leg.SLABreaches != 0 || leg.Count != 0 || len(leg.Caveats) == 0 {
		t.Fatalf("expected count gap with a caveat, got breaches=%d count=%d caveats=%v", leg.SLABreaches, leg.Count, leg.Caveats)
	}
}

func countPoint(stage, bucket, currency string, count int64, at time.Time) emit.MetricPoint {
	return emit.MetricPoint{
		Name:   "biz_inflight_count",
		Labels: map[string]string{"flow": "invoice.pay", "stage": stage, "age_bucket": bucket, "currency": currency},
		Value:  count, At: at,
	}
}

func TestDeferredExactCountsFromCountGauge(t *testing.T) {
	// With the companion count gauge (ADR-0012), Count and SLABreaches are exact
	// and the caveat is gone. SLABreaches counts every breach (past deadline),
	// not only the "lost" ones.
	at := win.To.Add(-time.Minute)
	metrics := []emit.MetricPoint{
		inflightPoint("capture", "5m-30m", "USD", 1000, at), countPoint("capture", "5m-30m", "USD", 10, at), // not breached
		inflightPoint("capture", "gt2h", "USD", 500, at), countPoint("capture", "gt2h", "USD", 5, at), // breached -> lost
		inflightPoint("capture", "30m-2h", "EUR", 700, at), countPoint("capture", "30m-2h", "EUR", 7, at), // breached -> lost
		inflightPoint("settle", "gt2h", "USD", 9000, at), countPoint("settle", "gt2h", "USD", 3, at), // settle P1D: not breached at gt2h
	}
	q := memq.New(memq.WithMetrics(metrics))
	leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}

	if leg.Count != 25 {
		t.Fatalf("Count = %d, want 25 (10+5+7+3)", leg.Count)
	}

	// Breaches: capture gt2h (5) + capture 30m-2h (7) = 12; settle gt2h and
	// capture 5m-30m are under their deadlines.
	if leg.SLABreaches != 12 {
		t.Fatalf("SLABreaches = %d, want 12 (all breaches, at_risk included)", leg.SLABreaches)
	}

	for _, c := range leg.Caveats {
		if strings.Contains(c, "COUNT") {
			t.Fatalf("count gauge present — the count-unavailable caveat must be gone: %v", leg.Caveats)
		}
	}

	// Value legs still correct alongside the counts.
	if leg.ByCurrency["USD"] != 10500 || leg.ProjectedLostMinor["EUR"] != 700 {
		t.Fatalf("value legs wrong: ByCurrency=%v projectedLost=%v", leg.ByCurrency, leg.ProjectedLostMinor)
	}
}

func TestDeferredAtRiskBreachCountsButIsNotProjectedLost(t *testing.T) {
	// A stage that is at_risk (not lost) and past its deadline is a breach — it
	// counts toward SLABreaches — but is not projected loss. The reference
	// registry cannot produce this (its only at_risk stage, settle P1D, can
	// never breach via the 120m top bucket), so parse a flow with an at_risk
	// deadline a bucket can cross.
	reg, err := registry.Parse([]byte(`version: 1
segments: [smb]
flows:
  hold.flow:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: hold, signals: ["queue:hold.q"] }
    sla:
      hold: { deadline: PT30M, on_breach: at_risk }
    estimator: { default_minor: 100 }
    baseline: { seasonality: hour_of_week, lookback_weeks: 8 }
    recovery: { model: usage_loss_curve, recovered_fraction: 0.5, within: PT2H }
    reconcile: { source: "sql:hold.ledger" }
`))
	if err != nil {
		t.Fatal(err)
	}

	at := win.To.Add(-time.Minute)
	hold := func(count, value int64, bucket string) []emit.MetricPoint {
		lbl := map[string]string{"flow": "hold.flow", "stage": "hold", "age_bucket": bucket, "currency": "USD"}
		return []emit.MetricPoint{
			{Name: "biz_inflight_value", Labels: lbl, Value: value, At: at},
			{Name: "biz_inflight_count", Labels: lbl, Value: count, At: at},
		}
	}
	var metrics []emit.MetricPoint
	metrics = append(metrics, hold(4, 4000, "gt2h")...)   // 120m >= 30m: breached (at_risk)
	metrics = append(metrics, hold(9, 9000, "5m-30m")...) // 5m < 30m: not breached
	q := memq.New(memq.WithMetrics(metrics))
	leg, err := Deferred(context.Background(), &reg, q, Request{Window: win, Flows: []string{"hold.flow"}})
	if err != nil {
		t.Fatal(err)
	}

	if leg.Count != 13 {
		t.Fatalf("Count = %d, want 13 (4 + 9)", leg.Count)
	}

	if leg.SLABreaches != 4 {
		t.Fatalf("SLABreaches = %d, want 4 (the at_risk breach counts)", leg.SLABreaches)
	}

	if len(leg.ProjectedLostMinor) != 0 {
		t.Fatalf("at_risk breach is NOT projected loss, got %v", leg.ProjectedLostMinor)
	}
}

func TestDeferredAtRiskIsNotProjectedLost(t *testing.T) {
	at := win.To.Add(-time.Minute)
	// settle SLA is P1D (1440m) -> at_risk; even a gt2h bucket is neither past
	// the deadline nor a "lost" policy, so projected-lost stays empty.
	metrics := []emit.MetricPoint{inflightPoint("settle", "gt2h", "USD", 9000, at)}
	q := memq.New(memq.WithMetrics(metrics))
	leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}

	if len(leg.ProjectedLostMinor) != 0 {
		t.Fatalf("at_risk/under-deadline value must not be projected-lost, got %v", leg.ProjectedLostMinor)
	}

	if leg.ByCurrency["USD"] != 9000 {
		t.Fatalf("still counted as deferred value: %v", leg.ByCurrency)
	}
}

func TestDeferredEdgeCases(t *testing.T) {
	at := win.To.Add(-time.Minute)
	t.Run("nil registry: value counted, no projected-lost", func(t *testing.T) {
		q := memq.New(memq.WithMetrics([]emit.MetricPoint{inflightPoint("capture", "gt2h", "USD", 500, at)}))
		leg, err := Deferred(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}})
		if err != nil {
			t.Fatal(err)
		}

		if leg.ByCurrency["USD"] != 500 {
			t.Fatalf("value = %v, want 500", leg.ByCurrency)
		}

		if len(leg.ProjectedLostMinor) != 0 {
			t.Fatalf("nil registry cannot know SLAs; projected-lost must be empty, got %v", leg.ProjectedLostMinor)
		}
	})
	t.Run("no flows: scope-only aggregates all flows", func(t *testing.T) {
		q := memq.New(memq.WithMetrics([]emit.MetricPoint{inflightPoint("capture", "5m-30m", "USD", 300, at)}))
		leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win}) // no Flows
		if err != nil {
			t.Fatal(err)
		}

		if leg.ByCurrency["USD"] != 300 {
			t.Fatalf("scope-only must still read the gauge, got %v", leg.ByCurrency)
		}
	})
	t.Run("zero level does not create a bucket entry", func(t *testing.T) {
		q := memq.New(memq.WithMetrics([]emit.MetricPoint{inflightPoint("capture", "gt2h", "USD", 0, at)}))
		leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
		if err != nil {
			t.Fatal(err)
		}

		if len(leg.ByAgeBucket) != 0 || len(leg.ByCurrency) != 0 {
			t.Fatalf("a zero level must not create entries, got buckets=%v cur=%v", leg.ByAgeBucket, leg.ByCurrency)
		}
	})
}

func TestDeferredNoMetricsErrors(t *testing.T) {
	q := memq.New(memq.WithCaps(query.Caps{Events: true}))
	if _, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win}); err == nil {
		t.Fatal("deferred without a metric source must error")
	}
}

// TestDeferredMatchesGoldenQueueScenario runs a capture consumer-stall through
// the in-memory querier and checks deferred totals, per-bucket ages, and
// projected-lost against ground truth computed independently from the ledger.
func TestDeferredMatchesGoldenQueueScenario(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) // Monday
	end := start.Add(3 * time.Hour)
	res := checkout.Run(checkout.Config{
		Seed:  11,
		Start: start,
		End:   end,
		Faults: []checkout.FaultSpec{{
			Kind:  checkout.FaultConsumerStall,
			From:  start.Add(30 * time.Minute),
			To:    end, // stall to the end so txns pile up in the capture queue
			Queue: checkout.QueueCapture,
		}},
	})

	// Ground truth computed INDEPENDENTLY of the leg: the deadline and policy
	// are read from the registry (not hardcoded), the bucket floors use a
	// local duration table (not the leg's package map), and breach is the
	// bucket-floor-vs-deadline rule the leg documents.
	reg := testRegistry(t)
	f, ok := reg.Flow("invoice.pay")
	if !ok {
		t.Fatal("registry missing invoice.pay")
	}

	capSLA := f.SLA["capture"]
	capLost := capSLA.OnBreach == registry.BreachLost
	localFloor := map[string]time.Duration{
		"lt1m": 0, "1m-5m": time.Minute, "5m-30m": 5 * time.Minute, "30m-2h": 30 * time.Minute, "gt2h": 120 * time.Minute,
	}
	wantByCur := map[string]int64{}
	wantBucket := map[string]int64{}
	wantProjLost := map[string]int64{}
	for _, tx := range res.Ledger.Txns {
		if tx.State != checkout.StateAuthed || tx.AuthedAt.IsZero() || tx.AuthedAt.After(end) {
			continue
		}

		bucket := emit.AgeBucketFor(end.Sub(tx.AuthedAt))
		wantByCur[tx.Currency] += tx.AmountMinor
		wantBucket[bucket] += tx.AmountMinor
		if capLost && localFloor[bucket] >= capSLA.Deadline {
			wantProjLost[tx.Currency] += tx.AmountMinor
		}
	}

	if wantByCur["USD"] == 0 {
		t.Fatal("consumer-stall produced no in-flight capture value; adjust the scenario")
	}

	q := testkit.QuerierFromResult(res)
	leg, err := Deferred(context.Background(), reg, q,
		Request{Window: query.TimeRange{From: start, To: end.Add(time.Second)}, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}

	if leg.ByCurrency["USD"] != wantByCur["USD"] {
		t.Fatalf("deferred USD = %d, want %d", leg.ByCurrency["USD"], wantByCur["USD"])
	}

	for bucket, want := range wantBucket {
		got := leg.ByAgeBucket[bucket]["USD"]
		if got != want {
			t.Fatalf("bucket %s USD = %d, want %d", bucket, got, want)
		}
	}

	if leg.ProjectedLostMinor["USD"] != wantProjLost["USD"] {
		t.Fatalf("projected-lost USD = %d, want %d", leg.ProjectedLostMinor["USD"], wantProjLost["USD"])
	}
}
