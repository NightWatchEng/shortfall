// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"reflect"
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

func TestDeferredEventsOnlyBackendWithNoEventsIsEmptyNotAnError(t *testing.T) {
	// An events-only backend grounds the leg from events (ADR-0019); with no
	// deferred events it is an empty leg carrying the events-derived caveat,
	// never an error — that is reserved for a backend serving neither signal.
	q := memq.New(memq.WithCaps(query.Caps{Events: true}))
	leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win})
	if err != nil {
		t.Fatalf("events-only backend must ground the leg: %v", err)
	}

	if leg.Unavailable || leg.Count != 0 || len(leg.ByCurrency) != 0 || len(leg.Caveats) != 1 {
		t.Fatalf("want an empty events-derived leg with one caveat, got %+v", leg)
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

// deferredEvent builds one outcome event for the events-derived deferred
// path: flow invoice.pay, USD at exponent 2, entity and stage as given.
func deferredEvent(entity, stage string, result biz.Result, amount int64, at time.Time) biz.Outcome {
	return biz.Outcome{
		At: at, Stage: stage, Result: result, Source: "test",
		VC: biz.ValueContext{
			Flow: "invoice.pay", EntityID: entity, CustomerID: "h:c1", Segment: "smb",
			Money: biz.Money{Amount: amount, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
		},
	}
}

// eventsWindow is a three-hour window, long enough that every age bucket
// and the two-hour lookback can be exercised from event times alone.
var eventsWindow = query.TimeRange{
	From: time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC),
	To:   time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
}

func TestDeferredFromEventsResolution(t *testing.T) {
	// One entity per case, judged alone, so a wrong resolution branch fails
	// by name. The reference registry orders auth → capture → settle, with
	// capture PT30M lost and settle P1D at_risk.
	to := eventsWindow.To
	cases := []struct {
		name          string
		events        []biz.Outcome
		wantCount     int64
		wantUSD       int64
		wantBucket    string
		wantBreaches  int64
		wantProjected int64
	}{
		{
			name:      "deferred three hours ago, never resolved: gt2h, breached, projected lost",
			events:    []biz.Outcome{deferredEvent("e", "capture", biz.ResultDeferred, 1000, to.Add(-3*time.Hour))},
			wantCount: 1, wantUSD: 1000, wantBucket: "gt2h", wantBreaches: 1, wantProjected: 1000,
		},
		{
			name: "retried deferral with two amounts counts once at the larger",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultDeferred, 300, to.Add(-10*time.Minute)),
				deferredEvent("e", "capture", biz.ResultDeferred, 500, to.Add(-8*time.Minute)),
			},
			wantCount: 1, wantUSD: 500, wantBucket: "5m-30m",
		},
		{
			name: "success at the same stage resolves",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultDeferred, 700, to.Add(-40*time.Minute)),
				deferredEvent("e", "capture", biz.ResultSuccess, 700, to.Add(-20*time.Minute)),
			},
		},
		{
			name:      "deferred at settle: at_risk SLA breaches nothing and projects nothing",
			events:    []biz.Outcome{deferredEvent("e", "settle", biz.ResultDeferred, 2000, to.Add(-50*time.Minute))},
			wantCount: 1, wantUSD: 2000, wantBucket: "30m-2h",
		},
		{
			name: "failed at a later stage resolves an earlier deferral",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultDeferred, 900, to.Add(-45*time.Minute)),
				deferredEvent("e", "settle", biz.ResultFailed, 900, to.Add(-15*time.Minute)),
			},
		},
		{
			name: "success at an earlier stage does not resolve a later deferral",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultSuccess, 400, to.Add(-30*time.Minute)),
				deferredEvent("e", "settle", biz.ResultDeferred, 400, to.Add(-25*time.Minute)),
			},
			wantCount: 1, wantUSD: 400, wantBucket: "5m-30m",
		},
		{
			name: "a later-stage deferral resolves the earlier one: counted once, aged from the later entry",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultDeferred, 600, to.Add(-70*time.Minute)),
				deferredEvent("e", "settle", biz.ResultDeferred, 600, to.Add(-3*time.Minute)),
			},
			wantCount: 1, wantUSD: 600, wantBucket: "1m-5m",
		},
		{
			name: "abandoned at the same stage resolves",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultDeferred, 800, to.Add(-12*time.Minute)),
				deferredEvent("e", "capture", biz.ResultAbandoned, 800, to.Add(-6*time.Minute)),
			},
		},
		{
			name: "unknown is not terminal",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultDeferred, 150, to.Add(-12*time.Minute)),
				deferredEvent("e", "capture", biz.ResultUnknown, 150, to.Add(-6*time.Minute)),
			},
			wantCount: 1, wantUSD: 150, wantBucket: "5m-30m",
		},
		{
			// The documented limit: the events path knows stage order, not
			// time order, so a failure followed by a retried deferral at the
			// same stage reads as resolved.
			name: "a failure before a retried deferral at the same stage is not seen (documented limit)",
			events: []biz.Outcome{
				deferredEvent("e", "capture", biz.ResultFailed, 250, to.Add(-50*time.Minute)),
				deferredEvent("e", "capture", biz.ResultDeferred, 250, to.Add(-10*time.Minute)),
			},
		},
		{
			// Bucket floors are left-closed, as on the gauge path: exactly
			// thirty minutes old is 30m-2h, and the PT30M capture SLA is
			// breached there.
			name:      "exactly thirty minutes old sits on the 30m floor and breaches",
			events:    []biz.Outcome{deferredEvent("e", "capture", biz.ResultDeferred, 100, to.Add(-30*time.Minute))},
			wantCount: 1, wantUSD: 100, wantBucket: "30m-2h", wantBreaches: 1, wantProjected: 100,
		},
		{
			name:      "one second short of thirty minutes is 5m-30m and does not breach",
			events:    []biz.Outcome{deferredEvent("e", "capture", biz.ResultDeferred, 100, to.Add(-30*time.Minute).Add(time.Second))},
			wantCount: 1, wantUSD: 100, wantBucket: "5m-30m",
		},
		{
			// A stage the registry does not order (a "dispute" the flow never
			// declared) resolves only on its own terminal: capture is not
			// "later" than it, because neither position is known.
			name: "an unordered stage is not resolved by an ordered one",
			events: []biz.Outcome{
				deferredEvent("e", "dispute", biz.ResultDeferred, 320, to.Add(-12*time.Minute)),
				deferredEvent("e", "capture", biz.ResultFailed, 320, to.Add(-6*time.Minute)),
			},
			wantCount: 1, wantUSD: 320, wantBucket: "5m-30m",
		},
		{
			name: "an unordered stage is resolved by its own terminal",
			events: []biz.Outcome{
				deferredEvent("e", "dispute", biz.ResultDeferred, 320, to.Add(-12*time.Minute)),
				deferredEvent("e", "dispute", biz.ResultSuccess, 320, to.Add(-6*time.Minute)),
			},
		},
		{
			name:      "exactly two hours old is gt2h",
			events:    []biz.Outcome{deferredEvent("e", "capture", biz.ResultDeferred, 100, to.Add(-2*time.Hour))},
			wantCount: 1, wantUSD: 100, wantBucket: "gt2h", wantBreaches: 1, wantProjected: 100,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := memq.New(memq.WithEvents(c.events), memq.WithCaps(query.Caps{Events: true}))
			leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: eventsWindow, Flows: []string{"invoice.pay"}})
			if err != nil {
				t.Fatal(err)
			}

			if leg.Unavailable || leg.Evidence != EvidenceDeterministic || len(leg.Caveats) != 1 || !strings.Contains(leg.Caveats[0], "events") {
				t.Fatalf("leg must be a measured, events-derived leg with one caveat: %+v", leg)
			}

			if leg.Count != c.wantCount || leg.ByCurrency["USD"] != c.wantUSD {
				t.Fatalf("count %d USD %d, want %d / %d", leg.Count, leg.ByCurrency["USD"], c.wantCount, c.wantUSD)
			}

			if c.wantBucket != "" && leg.ByAgeBucket[c.wantBucket]["USD"] != c.wantUSD {
				t.Fatalf("ByAgeBucket = %v, want USD %d in %s", leg.ByAgeBucket, c.wantUSD, c.wantBucket)
			}

			if leg.SLABreaches != c.wantBreaches || leg.ProjectedLostMinor["USD"] != c.wantProjected {
				t.Fatalf("breaches %d projected %v, want %d / USD %d", leg.SLABreaches, leg.ProjectedLostMinor, c.wantBreaches, c.wantProjected)
			}
		})
	}
}

func TestDeferredSourcePrecedence(t *testing.T) {
	to := eventsWindow.To
	deferred := []biz.Outcome{deferredEvent("e1", "capture", biz.ResultDeferred, 1000, to.Add(-3*time.Hour))}
	gaugeZero := []emit.MetricPoint{inflightPoint("capture", "lt1m", "USD", 0, to.Add(-time.Minute))}
	gaugeLevel := []emit.MetricPoint{inflightPoint("capture", "5m-30m", "USD", 250, to.Add(-time.Minute))}

	cases := []struct {
		name        string
		q           query.Querier
		wantUSD     int64
		wantEvents  bool // the events-derived caveat present
		wantUnavail bool
		wantErr     bool
	}{
		{"gauge with a level wins over events", memq.New(memq.WithEvents(deferred), memq.WithMetrics(gaugeLevel)), 250, false, false, false},
		{"gauge at level zero still wins: the tracker saw Done", memq.New(memq.WithEvents(deferred), memq.WithMetrics(gaugeZero)), 0, false, false, false},
		{"metrics backend with no gauge series falls to events", memq.New(memq.WithEvents(deferred)), 1000, true, false, false},
		{"events-only backend grounds from events", memq.New(memq.WithEvents(deferred), memq.WithCaps(query.Caps{Events: true})), 1000, true, false, false},
		{"metrics-only backend with no gauge stays an empty measured leg", memq.New(memq.WithCaps(query.Caps{Metrics: true})), 0, false, false, false},
		{"neither signal is an error", nullQuerier{}, 0, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			leg, err := Deferred(context.Background(), testRegistry(t), c.q, Request{Window: eventsWindow, Flows: []string{"invoice.pay"}})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}

			if c.wantErr {
				return
			}

			if leg.ByCurrency["USD"] != c.wantUSD {
				t.Fatalf("USD = %d, want %d (caveats %v)", leg.ByCurrency["USD"], c.wantUSD, leg.Caveats)
			}

			gotEvents := false
			for _, cv := range leg.Caveats {
				if strings.Contains(cv, "events") {
					gotEvents = true
				}
			}

			if gotEvents != c.wantEvents {
				t.Fatalf("events caveat present = %v, want %v: %v", gotEvents, c.wantEvents, leg.Caveats)
			}
		})
	}
}

func TestDeferredFromEventsLookback(t *testing.T) {
	// The lookback is max(2h, the flow's longest SLA deadline): the reference
	// registry's settle SLA is P1D, so backlog deferred a day before the
	// window start is still seen; older than that is not.
	to := eventsWindow.To
	cases := []struct {
		name    string
		at      time.Time
		wantUSD int64
	}{
		{"deferred inside the window", to.Add(-10 * time.Minute), 100},
		{"deferred before the window, inside the lookback", eventsWindow.From.Add(-20 * time.Hour), 100},
		{"deferred before the lookback", eventsWindow.From.Add(-25 * time.Hour), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := memq.New(memq.WithEvents([]biz.Outcome{deferredEvent("e1", "settle", biz.ResultDeferred, 100, c.at)}),
				memq.WithCaps(query.Caps{Events: true}))
			leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: eventsWindow, Flows: []string{"invoice.pay"}})
			if err != nil {
				t.Fatal(err)
			}

			if leg.ByCurrency["USD"] != c.wantUSD {
				t.Fatalf("USD = %d, want %d", leg.ByCurrency["USD"], c.wantUSD)
			}
		})
	}
}

// TestDeferredFromEventsAgreesWithTracker is the events path's fence: the
// leg derived from deferred outcome events alone must equal the leg read
// from the gauge the real InFlightTracker published for the same ledger —
// value, buckets, projected-lost, count and breaches. A capture stall
// exercises breaches and projected loss; a healthy run exercises the settle
// backlog, where a transaction deferred at capture and again at settle must
// count once. Both legs come from one run, so the assertion is
// platform-self-consistent.
func TestDeferredFromEventsAgreesWithTracker(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Hour)
	cases := []struct {
		name         string
		faults       []checkout.FaultSpec
		wantBreaches bool
	}{
		{"capture consumer stall", []checkout.FaultSpec{{
			Kind:  checkout.FaultConsumerStall,
			From:  start.Add(30 * time.Minute),
			To:    end,
			Queue: checkout.QueueCapture,
		}}, true},
		{"healthy pipeline, settle backlog only", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := checkout.Run(checkout.Config{Seed: 11, Start: start, End: end, Faults: c.faults})
			reg := testRegistry(t)
			req := Request{Window: query.TimeRange{From: start, To: end.Add(time.Second)}, Flows: []string{"invoice.pay"}}

			gauge, err := Deferred(context.Background(), reg, testkit.QuerierFromResult(res), req)
			if err != nil {
				t.Fatal(err)
			}

			eventsOnly := memq.New(memq.WithEvents(testkit.EventsFromResult(res)), memq.WithCaps(query.Caps{Events: true}))
			events, err := Deferred(context.Background(), reg, eventsOnly, req)
			if err != nil {
				t.Fatal(err)
			}

			if gauge.Count == 0 || (gauge.SLABreaches == 0) == c.wantBreaches || len(gauge.Caveats) != 0 {
				t.Fatalf("gauge leg must be non-trivial (breaches=%v) and caveat-free for this fence to prove anything: %+v", c.wantBreaches, gauge)
			}

			if len(events.Caveats) != 1 || !strings.Contains(events.Caveats[0], "events-derived") {
				t.Fatalf("events leg must carry the events-derived caveat: %v", events.Caveats)
			}

			events.Caveats = nil
			if !reflect.DeepEqual(gauge, events) {
				t.Fatalf("events-derived leg disagrees with the tracker's gauge:\ngauge:  %+v\nevents: %+v", gauge, events)
			}
		})
	}
}

// TestDeferredFromEventsSeesAPaymentsServiceOutage is the worked webhook
// example's failure direction: the Lambda records ingest as deferred when
// payments-service answers 5xx, payments-service itself is down and publishes
// no gauge, and the events store is all the report has. The leg must show
// the backlog, aged and projected against the ingest stage's SLA — the
// stage the deferral was recorded at.
func TestDeferredFromEventsSeesAPaymentsServiceOutage(t *testing.T) {
	reg, err := registry.Parse([]byte(`
version: 1
segments: [smb, enterprise]
flows:
  payment.webhook:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: ingest,  signals: ["webhook:payment_intent.succeeded"] }
      - { name: process, signals: ["http:POST /internal/webhooks/process"] }
    sla:
      ingest: { deadline: PT30M, on_breach: lost }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 8 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.9, within: PT2H }
    reconcile: { source: "stripe:payment_intents" }
`))
	if err != nil {
		t.Fatal(err)
	}

	to := eventsWindow.To
	webhook := func(id string, amount int64, at time.Time) biz.Outcome {
		return biz.Outcome{At: at, Stage: "ingest", Result: biz.ResultDeferred, Source: "lambda", VC: biz.ValueContext{
			Flow: "payment.webhook", EntityID: id, CustomerID: "h:c9",
			Money: biz.Money{Amount: amount, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
		}}
	}
	events := []biz.Outcome{
		webhook("pi_1", 5000, to.Add(-50*time.Minute)), // past the 30m SLA: projected lost
		webhook("pi_2", 7000, to.Add(-10*time.Minute)),
		webhook("pi_2", 7000, to.Add(-4*time.Minute)), // the provider redelivered; one entity
	}
	q := memq.New(memq.WithEvents(events), memq.WithCaps(query.Caps{Events: true}))
	leg, err := Deferred(context.Background(), &reg, q, Request{Window: eventsWindow, Flows: []string{"payment.webhook"}})
	if err != nil {
		t.Fatal(err)
	}

	if leg.ByCurrency["USD"] != 12000 || leg.Count != 2 {
		t.Fatalf("deferred = %v count %d, want USD 12000 over 2 entities", leg.ByCurrency, leg.Count)
	}

	if leg.ProjectedLostMinor["USD"] != 5000 || leg.SLABreaches != 1 || leg.OldestAgeMinutes != 30 {
		t.Fatalf("projected lost %v breaches %d oldest %d, want USD 5000 / 1 / 30", leg.ProjectedLostMinor, leg.SLABreaches, leg.OldestAgeMinutes)
	}
}

// TestDeferredFromEventsLookbackIsPerFlow pins that a flow's lookback is its
// own longest SLA, never a co-requested flow's: a.flow (PT30M) keeps the
// two-hour floor whether or not b.flow (P1D) shares the request, so a
// three-hour-old a.flow deferral is unseen either way.
func TestDeferredFromEventsLookbackIsPerFlow(t *testing.T) {
	reg, err := registry.Parse([]byte(`
version: 1
segments: [smb]
flows:
  a.flow:
    money: { kind: fee }
    stages: [{ name: pay, signals: ["http:POST /a"] }]
    sla: { pay: { deadline: PT30M, on_breach: lost } }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "sql:a" }
  b.flow:
    money: { kind: fee }
    stages: [{ name: pay, signals: ["http:POST /b"] }]
    sla: { pay: { deadline: P1D, on_breach: at_risk } }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "sql:b" }
`))
	if err != nil {
		t.Fatal(err)
	}

	old := eventsWindow.From.Add(-3 * time.Hour)
	ev := func(flow string) biz.Outcome {
		return biz.Outcome{At: old, Stage: "pay", Result: biz.ResultDeferred, Source: "test", VC: biz.ValueContext{
			Flow: flow, EntityID: "e-" + flow, CustomerID: "h:c1",
			Money: biz.Money{Amount: 1000, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
		}}
	}
	q := memq.New(memq.WithEvents([]biz.Outcome{ev("a.flow"), ev("b.flow")}), memq.WithCaps(query.Caps{Events: true}))
	cases := []struct {
		name    string
		flows   []string
		wantUSD int64
	}{
		{"a.flow alone: unseen past its two-hour lookback", []string{"a.flow"}, 0},
		{"a.flow with b.flow: still unseen; b.flow's day-long lookback is its own", []string{"a.flow", "b.flow"}, 1000},
		{"b.flow alone: seen within its day-long lookback", []string{"b.flow"}, 1000},
		{"no flow named: one query at the registry's longest lookback sees both", nil, 2000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			leg, err := Deferred(context.Background(), &reg, q, Request{Window: eventsWindow, Flows: c.flows})
			if err != nil {
				t.Fatal(err)
			}

			if leg.ByCurrency["USD"] != c.wantUSD {
				t.Fatalf("USD = %d, want %d", leg.ByCurrency["USD"], c.wantUSD)
			}
		})
	}
}
