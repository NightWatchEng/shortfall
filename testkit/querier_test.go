// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
)

// TestTelemetryOutcomeMapping pins each state's mapping to a telemetry
// outcome, independent of a harness run — including that abandoned and
// in-flight states are not telemetry-visible.
func TestTelemetryOutcomeMapping(t *testing.T) {
	t0 := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	tx := func(state checkout.State, authed, captured, settled time.Time) checkout.Txn {
		return checkout.Txn{
			State:      state,
			CreatedAt:  t0,
			AuthedAt:   authed,
			CapturedAt: captured,
			SettledAt:  settled,
		}
	}
	cases := []struct {
		name        string
		txn         checkout.Txn
		wantVisible bool
		wantStage   string
		wantResult  biz.Result
		wantAt      time.Time
	}{
		{"settled", tx(checkout.StateSettled, t0.Add(1*time.Minute), t0.Add(2*time.Minute), t0.Add(5*time.Minute)), true, "settle", biz.ResultSuccess, t0.Add(5 * time.Minute)},
		{"capture failed", tx(checkout.StateCapFail, t0.Add(1*time.Minute), t0.Add(2*time.Minute), time.Time{}), true, "capture", biz.ResultFailed, t0.Add(2 * time.Minute)},
		{"capture failed falls back to authed time", tx(checkout.StateCapFail, t0.Add(1*time.Minute), time.Time{}, time.Time{}), true, "capture", biz.ResultFailed, t0.Add(1 * time.Minute)},
		{"auth failed", tx(checkout.StateAuthFail, t0.Add(1*time.Minute), time.Time{}, time.Time{}), true, "auth", biz.ResultFailed, t0.Add(1 * time.Minute)},
		{"auth failed falls back to created time", tx(checkout.StateAuthFail, time.Time{}, time.Time{}, time.Time{}), true, "auth", biz.ResultFailed, t0},
		{"abandoned is invisible to telemetry", tx(checkout.StateAbandoned, time.Time{}, time.Time{}, time.Time{}), false, "", "", time.Time{}},
		{"created is in-flight, invisible", tx(checkout.StateCreated, time.Time{}, time.Time{}, time.Time{}), false, "", "", time.Time{}},
		{"authed is in-flight, invisible", tx(checkout.StateAuthed, t0.Add(1*time.Minute), time.Time{}, time.Time{}), false, "", "", time.Time{}},
		{"captured is in-flight, invisible", tx(checkout.StateCaptured, t0.Add(1*time.Minute), t0.Add(2*time.Minute), time.Time{}), false, "", "", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stage, result, at, visible := telemetryOutcome(c.txn)
			if visible != c.wantVisible {
				t.Fatalf("visible = %v, want %v", visible, c.wantVisible)
			}

			if !visible {
				return
			}

			if stage != c.wantStage || result != c.wantResult || !at.Equal(c.wantAt) {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", stage, result, at, c.wantStage, c.wantResult, c.wantAt)
			}
		})
	}
}

// TestQuerierFromResultServesLedgerWithNoBackend proves the whole read path
// is exercisable from a harness run with zero external processes: a run's
// ground-truth ledger becomes an in-memory querier whose event and metric
// answers match the ledger's terminal transactions.
func TestQuerierFromResultServesLedgerWithNoBackend(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) // a Monday
	// A slice of 5xx failures keeps the auth-failed branch of the entry
	// assertion non-vacuous — with no fault, StateAuthFail never occurs and
	// the auth-failed term would be asserted against a guaranteed-empty set.
	res := checkout.Run(checkout.Config{
		Seed:  42,
		Start: start,
		End:   start.Add(2 * time.Hour),
		Faults: []checkout.FaultSpec{{
			Kind: checkout.FaultAPI5xx, Rate: 0.2,
			From: start.Add(30 * time.Minute), To: start.Add(90 * time.Minute),
		}},
	})
	if len(res.Ledger.Txns) == 0 {
		t.Fatal("harness produced an empty ledger")
	}

	// Ground truth by hand: terminal txns and their failed value, plus the
	// flow entries (txns that authed — each adds one entry-stage point).
	var wantTerminal, wantEntered, wantAuthFailed int
	var wantFailedValueUSD int64
	var currencies = map[string]bool{}
	for _, txn := range res.Ledger.Txns {
		if !txn.AuthedAt.IsZero() {
			wantEntered++
		}

		_, result, _, visible := telemetryOutcome(txn)
		if !visible {
			continue
		}

		wantTerminal++
		currencies[txn.Currency] = true
		if txn.State == checkout.StateAuthFail {
			wantAuthFailed++
		}

		if result == biz.Result("failed") && txn.Currency == "USD" {
			wantFailedValueUSD += txn.AmountMinor
		}
	}

	q := QuerierFromResult(res)
	ctx := context.Background()
	full := query.TimeRange{From: start, To: start.Add(24 * time.Hour)}

	// Metrics: the total biz_txn_total is one terminal point per terminal
	// txn plus one entry-stage point per txn that entered the flow, and the
	// entry-stage sum (over outcomes) counts every entry — successes via
	// their entry point, auth failures via their terminal point.
	sumTxn := func(filters map[string]string) int {
		series, err := q.QueryMetric(ctx, query.Query{
			Metric: "biz_txn_total", Agg: query.AggSum, Filters: filters, Range: full,
		})
		if err != nil {
			t.Fatal(err)
		}

		var got float64
		for _, s := range series {
			for _, p := range s.Points {
				got += p.Value
			}
		}

		return int(got)
	}
	if got := sumTxn(nil); got != wantTerminal+wantEntered {
		t.Fatalf("biz_txn_total sum = %d, want %d (terminal %d + entered %d)", got, wantTerminal+wantEntered, wantTerminal, wantEntered)
	}

	if wantAuthFailed == 0 {
		t.Fatal("fixture produced no auth failures — the auth-failed half of the entry assertion would be vacuous")
	}

	if got := sumTxn(map[string]string{"stage": "auth"}); got != wantEntered+wantAuthFailed {
		t.Fatalf("entry-stage biz_txn_total sum = %d, want %d entries (entered %d + auth-failed %d)", got, wantEntered+wantAuthFailed, wantEntered, wantAuthFailed)
	}

	// Events: failed USD value via a currency-pinned, outcome-filtered event
	// query matches the hand-computed ground truth.
	groups, err := q.QueryEvents(ctx, query.EventQuery{
		Range:   full,
		Filters: map[string]string{"currency": "USD", "outcome": "failed"},
		GroupBy: []string{"currency"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotFailedUSD int64
	for _, g := range groups {
		gotFailedUSD += g.SumMinor
	}

	if gotFailedUSD != wantFailedValueUSD {
		t.Fatalf("failed USD value = %d, want %d", gotFailedUSD, wantFailedValueUSD)
	}

	// The querier serves both signals with no process running.
	if c := q.Capabilities(); !c.Metrics || !c.Events {
		t.Fatalf("caps = %+v, want both", c)
	}
}

// TestInFlightGaugeSnapshotVisibleWithinWindow pins the vacuous-gauge-parity
// regression: a biz_inflight_value snapshot stamped exactly at a query
// window's end To is invisible to the half-open [From, To) read on both sides
// (memq drops At>=To; the promql adapter reads last_over_time at To-1ms), so
// an at-To gauge parity check compares empty==empty and proves nothing.
// MetricsFromResultAt lets the harness snapshot strictly inside the window.
func TestInFlightGaugeSnapshotVisibleWithinWindow(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(50 * time.Minute)
	// A stalled capture queue guarantees a backlog of in-flight (StateAuthed)
	// transactions left at the run's end.
	res := checkout.Run(checkout.Config{
		Seed: 5, Start: start, End: end,
		Faults: []checkout.FaultSpec{{
			Kind: checkout.FaultConsumerStall, Queue: checkout.QueueCapture,
			From: start.Add(10 * time.Minute), To: start.Add(35 * time.Minute),
		}},
	})
	window := query.TimeRange{From: start, To: end}
	gaugeQ := query.Query{
		Metric:  "biz_inflight_value",
		GroupBy: []string{"age_bucket", "currency"},
		Range:   window,
	}
	ctx := context.Background()

	// Setup sanity: the stall must leave a backlog, else neither case proves
	// anything.
	sanity, _ := memq.New(memq.WithMetrics(InFlightPointsAt(res, end.Add(-time.Minute)))).
		QueryMetric(ctx, gaugeQ)
	if len(sanity) == 0 {
		t.Fatal("stall scenario left no in-flight backlog; test setup is broken")
	}

	cases := []struct {
		name     string
		gaugeAt  time.Time
		wantSeen bool
	}{
		{"snapshot at window end To is invisible (the bug)", end, false},
		{"snapshot one minute inside the window is visible", end.Add(-time.Minute), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			series, err := memq.New(memq.WithMetrics(MetricsFromResultAt(res, c.gaugeAt))).
				QueryMetric(ctx, gaugeQ)
			if err != nil {
				t.Fatal(err)
			}

			if seen := len(series) > 0; seen != c.wantSeen {
				t.Fatalf("gauge series seen = %v (%d series), want %v", seen, len(series), c.wantSeen)
			}
		})
	}
}
