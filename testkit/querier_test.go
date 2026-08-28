package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
)

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
		_, result, _, terminal := terminalOf(txn)
		if !terminal {
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
