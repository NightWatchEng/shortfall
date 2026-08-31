// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/testkit"
)

var win = query.TimeRange{
	From: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC),
	To:   time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
}

func evAt(min int, flow, entity string, result biz.Result, amount int64, currency string) biz.Outcome {
	return biz.Outcome{
		At:    win.From.Add(time.Duration(min) * time.Minute),
		Stage: "capture", Result: result,
		VC: biz.ValueContext{Flow: flow, EntityID: entity, Money: biz.Money{Amount: amount, Currency: currency, Exponent: 2}, Kind: biz.KindFee},
	}
}

func TestRealizedLegEventsPath(t *testing.T) {
	req := Request{Window: win, Flows: []string{"invoice.pay"}}
	cases := []struct {
		name      string
		events    []biz.Outcome
		wantCount int64
		wantByCur map[string]int64
	}{
		{
			name: "distinct failed entities sum once each",
			events: []biz.Outcome{
				evAt(1, "invoice.pay", "inv_1", biz.ResultFailed, 14900, "USD"),
				evAt(2, "invoice.pay", "inv_2", biz.ResultFailed, 100, "USD"),
			},
			wantCount: 2, wantByCur: map[string]int64{"USD": 15000},
		},
		{
			name: "duplicate failed events for one entity count once",
			events: []biz.Outcome{
				evAt(1, "invoice.pay", "inv_1", biz.ResultFailed, 14900, "USD"),
				evAt(2, "invoice.pay", "inv_1", biz.ResultFailed, 14900, "USD"), // cross-process dup
			},
			wantCount: 1, wantByCur: map[string]int64{"USD": 14900},
		},
		{
			name: "failed-then-recovered entity is excluded",
			events: []biz.Outcome{
				evAt(1, "invoice.pay", "inv_1", biz.ResultFailed, 14900, "USD"),
				evAt(5, "invoice.pay", "inv_1", biz.ResultSuccess, 14900, "USD"), // retry succeeded
				evAt(2, "invoice.pay", "inv_2", biz.ResultFailed, 500, "USD"),
			},
			wantCount: 1, wantByCur: map[string]int64{"USD": 500},
		},
		{
			name: "per-currency, never summed across currencies",
			events: []biz.Outcome{
				evAt(1, "invoice.pay", "inv_1", biz.ResultFailed, 14900, "USD"),
				evAt(2, "invoice.pay", "inv_2", biz.ResultFailed, 5000, "EUR"),
			},
			wantCount: 2, wantByCur: map[string]int64{"USD": 14900, "EUR": 5000},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := memq.New(memq.WithEvents(c.events))
			leg, err := RealizedLeg(context.Background(), nil, q, req)
			if err != nil {
				t.Fatal(err)
			}
			if leg.Count != c.wantCount {
				t.Fatalf("count = %d, want %d", leg.Count, c.wantCount)
			}
			if leg.Evidence != EvidenceDeterministic {
				t.Fatalf("evidence = %q, want deterministic", leg.Evidence)
			}
			if len(leg.Caveats) != 0 {
				t.Fatalf("events path must carry no caveat, got %v", leg.Caveats)
			}
			for cur, want := range c.wantByCur {
				if leg.ByCurrency[cur] != want {
					t.Fatalf("ByCurrency[%s] = %d, want %d", cur, leg.ByCurrency[cur], want)
				}
			}
			if len(leg.ByCurrency) != len(c.wantByCur) {
				t.Fatalf("ByCurrency = %v, want %v", leg.ByCurrency, c.wantByCur)
			}
		})
	}
}

func TestRealizedLegDeDupsDuplicatesExactly(t *testing.T) {
	// Per-entity de-dup takes the max single failed amount, not a mean
	// (ADR-0009). Identical redeliveries collapse to their exact value; an
	// entity with differing failed amounts collapses to the largest single
	// exposure — a real figure, never an average — with no caveat.
	cases := []struct {
		name    string
		amounts []int64
		want    int64
	}{
		{"identical redeliveries -> exact value", []int64{200, 200, 200}, 200},
		{"differing amounts -> max, not mean (100,101 would mean 100)", []int64{100, 101}, 101},
		{"even-division differing -> max, not the hidden mean 200", []int64{100, 300}, 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var events []biz.Outcome
			for i, amt := range c.amounts {
				events = append(events, evAt(i+1, "invoice.pay", "inv_1", biz.ResultFailed, amt, "USD"))
			}
			q := memq.New(memq.WithEvents(events))
			leg, err := RealizedLeg(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(leg.Caveats) != 0 {
				t.Fatalf("exact de-dup must not caveat, got %v", leg.Caveats)
			}
			if leg.Count != 1 {
				t.Fatalf("count = %d, want 1 (still one entity)", leg.Count)
			}
			if leg.ByCurrency["USD"] != c.want {
				t.Fatalf("USD = %d, want %d (max representative)", leg.ByCurrency["USD"], c.want)
			}
		})
	}
}

func TestRealizedLegScopeDoesNotOverrideOutcome(t *testing.T) {
	// A Scope carrying a reserved key must not hijack the leg's own filter.
	events := []biz.Outcome{
		evAt(1, "invoice.pay", "inv_1", biz.ResultFailed, 500, "USD"),
		evAt(2, "invoice.pay", "inv_2", biz.ResultSuccess, 900, "USD"),
	}
	q := memq.New(memq.WithEvents(events))
	leg, err := RealizedLeg(context.Background(), nil, q, Request{
		Window: win, Flows: []string{"invoice.pay"}, Scope: Scope{"outcome": "success"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// outcome=failed still wins, so only the failed 500 is realized.
	if leg.ByCurrency["USD"] != 500 {
		t.Fatalf("USD = %d, want 500 (scope must not override outcome)", leg.ByCurrency["USD"])
	}
}

func TestRealizedLegMetricsOnlyCarriesCaveat(t *testing.T) {
	metrics := []emit.MetricPoint{{
		Name: "biz_value_total",
		Labels: map[string]string{
			"flow":     "invoice.pay",
			"currency": "USD",
			"outcome":  "failed",
			"stage":    "capture",
			"kind":     "fee",
			"segment":  "smb",
		},
		Value: 15000, At: win.From.Add(time.Minute),
	}}
	// metrics-only backend: events unsupported.
	q := memq.New(
		memq.WithMetrics(metrics),
		memq.WithCaps(query.Caps{Metrics: true, Events: false, MetricHistoryWeeks: 8}),
	)
	leg, err := RealizedLeg(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}
	if leg.ByCurrency["USD"] != 15000 {
		t.Fatalf("USD = %d, want 15000", leg.ByCurrency["USD"])
	}
	if len(leg.Caveats) == 0 {
		t.Fatal("metrics-only path must carry a caveat in the Leg struct")
	}
	if leg.Evidence != EvidenceDeterministic {
		t.Fatalf("evidence = %q, want deterministic", leg.Evidence)
	}
}

func TestRealizedLegNoSourceErrors(t *testing.T) {
	q := memq.New(memq.WithCaps(query.Caps{})) // neither metrics nor events
	if _, err := RealizedLeg(context.Background(), nil, q, Request{Window: win}); err == nil {
		t.Fatal("a backend serving neither signal must error, not return a zero leg")
	}
}

// TestRealizedLegMatchesGoldenScenario runs a real harness incident through
// the in-memory querier and checks the events-path realized loss against the
// ground-truth ledger within 0.5%.
func TestRealizedLegMatchesGoldenScenario(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) // Monday
	res := checkout.Run(checkout.Config{
		Seed:  7,
		Start: start,
		End:   start.Add(3 * time.Hour),
		Faults: []checkout.FaultSpec{{
			Kind: checkout.FaultAPI5xx,
			From: start.Add(30 * time.Minute),
			To:   start.Add(90 * time.Minute),
			Rate: 0.5,
		}},
	})

	// Ground truth: de-duped failed value by entity, excluding recovered.
	succeeded := map[string]bool{}
	for _, tx := range res.Ledger.Txns {
		if tx.State == checkout.StateSettled {
			succeeded[tx.ID] = true
		}
	}
	wantUSD := int64(0)
	seen := map[string]bool{}
	for _, tx := range res.Ledger.Txns {
		if (tx.State == checkout.StateAuthFail || tx.State == checkout.StateCapFail) && !succeeded[tx.ID] && !seen[tx.ID] {
			seen[tx.ID] = true
			wantUSD += tx.AmountMinor
		}
	}
	if wantUSD == 0 {
		t.Fatal("scenario produced no realized loss; pick a fault that fails transactions")
	}

	q := testkit.QuerierFromResult(res)
	full := query.TimeRange{From: start, To: start.Add(24 * time.Hour)}
	leg, err := RealizedLeg(context.Background(), nil, q, Request{Window: full, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}
	// The events path is an exact integer sum, so it must match ground truth
	// exactly. This test covers the sum/feeder/memq integration on realistic
	// data; the de-dup and recovery-exclusion branches (which the harness
	// never triggers — one terminal state per unique txn) are covered by
	// TestRealizedLegEventsPath with synthetic events.
	got := leg.ByCurrency["USD"]
	if got != wantUSD {
		diff := float64(got-wantUSD) / float64(wantUSD) * 100
		t.Fatalf("realized USD = %d, want exactly %d (%.3f%% off)", got, wantUSD, diff)
	}
}
