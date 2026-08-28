package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
)

// TestTelemetryOutcomeMapping pins each state's mapping to a telemetry
// outcome, independent of a harness run — including that abandoned and
// in-flight states are NOT telemetry-visible.
func TestTelemetryOutcomeMapping(t *testing.T) {
	t0 := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	tx := func(state checkout.State, authed, captured, settled time.Time) checkout.Txn {
		return checkout.Txn{State: state, CreatedAt: t0, AuthedAt: authed, CapturedAt: captured, SettledAt: settled}
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
	res := checkout.Run(checkout.Config{
		Seed:  42,
		Start: start,
		End:   start.Add(2 * time.Hour),
	})
	if len(res.Ledger.Txns) == 0 {
		t.Fatal("harness produced an empty ledger")
	}

	// Ground truth: count terminal txns and their failed value, by hand.
	var wantTerminal int
	var wantFailedValueUSD int64
	var currencies = map[string]bool{}
	for _, txn := range res.Ledger.Txns {
		_, result, _, visible := telemetryOutcome(txn)
		if !visible {
			continue
		}
		wantTerminal++
		currencies[txn.Currency] = true
		if result == biz.Result("failed") && txn.Currency == "USD" {
			wantFailedValueUSD += txn.AmountMinor
		}
	}

	q := QuerierFromResult(res)
	ctx := context.Background()
	full := query.TimeRange{From: start, To: start.Add(24 * time.Hour)}

	// Events: total terminal count via distinct entity ids... simpler: sum
	// biz_txn_total over the window equals the terminal count.
	series, err := q.QueryMetric(ctx, query.Query{Metric: "biz_txn_total", Agg: query.AggSum, Range: full})
	if err != nil {
		t.Fatal(err)
	}
	var gotTxn float64
	for _, s := range series {
		for _, p := range s.Points {
			gotTxn += p.Value
		}
	}
	if int(gotTxn) != wantTerminal {
		t.Fatalf("biz_txn_total sum = %d, want %d terminal txns", int(gotTxn), wantTerminal)
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
