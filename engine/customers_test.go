package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/testkit"
)

func custEv(min int, customer, segment string, result biz.Result, amount int64, currency string) biz.Outcome {
	return biz.Outcome{
		At:    win.From.Add(time.Duration(min) * time.Minute),
		Stage: "capture", Result: result,
		VC: biz.ValueContext{
			Flow: "invoice.pay", EntityID: customer + "-e", CustomerID: customer, Segment: segment,
			Money: biz.Money{Amount: amount, Currency: currency, Exponent: 2}, Kind: biz.KindFee,
		},
	}
}

func TestCustomersNotAvailableWithoutEvents(t *testing.T) {
	q := memq.New(memq.WithCaps(query.Caps{Metrics: true})) // metrics-only
	leg, err := Customers(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if leg.NotAvailableReason == "" {
		t.Fatal("metrics-only backend must yield a NotAvailableReason, not a zero leg")
	}
	if leg.Distinct != 0 || len(leg.TopN) != 0 {
		t.Fatalf("unavailable leg must be empty, got %+v", leg)
	}
}

func TestCustomersDistinctSegmentAndTopN(t *testing.T) {
	events := []biz.Outcome{
		custEv(1, "h:c1", "smb", biz.ResultFailed, 100, "USD"),
		custEv(2, "h:c1", "smb", biz.ResultFailed, 400, "USD"), // same customer, adds up
		custEv(3, "h:c2", "enterprise", biz.ResultFailed, 900, "USD"),
		custEv(4, "h:c3", "smb", biz.ResultFailed, 50, "USD"),
		custEv(5, "h:c4", "smb", biz.ResultSuccess, 9999, "USD"), // success: not hit
	}
	q := memq.New(memq.WithEvents(events))
	leg, err := Customers(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if leg.Distinct != 3 {
		t.Fatalf("distinct = %d, want 3 (c1,c2,c3; c4 only succeeded)", leg.Distinct)
	}
	if leg.BySegment["smb"] != 2 || leg.BySegment["enterprise"] != 1 {
		t.Fatalf("BySegment = %v, want smb 2 enterprise 1", leg.BySegment)
	}
	if len(leg.TopN) != 2 {
		t.Fatalf("TopN len = %d, want 2 (limited)", len(leg.TopN))
	}
	// Ranked by loss: c2 (900) then c1 (500).
	if leg.TopN[0].CustomerID != "h:c2" || leg.TopN[0].ByCurrency["USD"] != 900 {
		t.Fatalf("top = %+v, want c2 900", leg.TopN[0])
	}
	if leg.TopN[1].CustomerID != "h:c1" || leg.TopN[1].ByCurrency["USD"] != 500 {
		t.Fatalf("second = %+v, want c1 500", leg.TopN[1])
	}
}

func TestCustomersTopNRanksByLargestCurrencyNotCrossSum(t *testing.T) {
	events := []biz.Outcome{
		// c1: 600 USD + 600 EUR (cross-sum 1200, but max-currency 600)
		custEv(1, "h:c1", "smb", biz.ResultFailed, 600, "USD"),
		custEv(2, "h:c1", "smb", biz.ResultFailed, 600, "EUR"),
		// c2: 800 USD only (max-currency 800 > 600)
		custEv(3, "h:c2", "smb", biz.ResultFailed, 800, "USD"),
	}
	q := memq.New(memq.WithEvents(events))
	leg, err := Customers(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	// c2 ranks first: cross-currency summing (which would put c1 at 1200) is
	// forbidden; ranking is by largest single-currency loss (c2 800 > c1 600).
	if leg.TopN[0].CustomerID != "h:c2" {
		t.Fatalf("top = %s, want h:c2 (max-currency ranking, no cross-sum)", leg.TopN[0].CustomerID)
	}
	// c1's ByCurrency preserves both currencies.
	var c1 CustomerImpact
	for _, ci := range leg.TopN {
		if ci.CustomerID == "h:c1" {
			c1 = ci
		}
	}
	if c1.ByCurrency["USD"] != 600 || c1.ByCurrency["EUR"] != 600 {
		t.Fatalf("c1 ByCurrency = %v, want USD 600 EUR 600", c1.ByCurrency)
	}
}

func TestCustomersEmptyWhenNoFailures(t *testing.T) {
	events := []biz.Outcome{custEv(1, "h:c1", "smb", biz.ResultSuccess, 100, "USD")}
	q := memq.New(memq.WithEvents(events))
	leg, err := Customers(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if leg.NotAvailableReason != "" {
		t.Fatal("events ARE available; no failures is a real zero, not NotAvailable")
	}
	if leg.Distinct != 0 || len(leg.TopN) != 0 {
		t.Fatalf("no failures -> empty, got %+v", leg)
	}
}

// TestCustomersMatchesGoldenScenario checks distinct + by-segment against
// ground truth from a real api-5xx incident ledger.
func TestCustomersMatchesGoldenScenario(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	res := checkout.Run(checkout.Config{
		Seed:  23,
		Start: start,
		End:   start.Add(3 * time.Hour),
		Faults: []checkout.FaultSpec{{
			Kind: checkout.FaultAPI5xx, From: start.Add(30 * time.Minute), To: start.Add(90 * time.Minute), Rate: 0.6,
		}},
	})
	// Ground truth: distinct customers with a telemetry-visible failure, by segment.
	distinct := map[string]string{} // customer -> segment
	for _, tx := range res.Ledger.Txns {
		if tx.State == checkout.StateAuthFail || tx.State == checkout.StateCapFail {
			distinct[tx.CustomerID] = string(tx.Segment)
		}
	}
	wantBySeg := map[string]int64{}
	for _, seg := range distinct {
		wantBySeg[seg]++
	}
	if len(distinct) == 0 {
		t.Fatal("scenario produced no failed customers")
	}

	q := testkit.QuerierFromResult(res)
	full := query.TimeRange{From: start, To: start.Add(24 * time.Hour)}
	leg, err := Customers(context.Background(), nil, q, Request{Window: full, Flows: []string{"invoice.pay"}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if leg.Distinct != int64(len(distinct)) {
		t.Fatalf("distinct = %d, want %d", leg.Distinct, len(distinct))
	}
	for seg, want := range wantBySeg {
		if leg.BySegment[seg] != want {
			t.Fatalf("BySegment[%s] = %d, want %d", seg, leg.BySegment[seg], want)
		}
	}
	if len(leg.TopN) == 0 || len(leg.TopN) > 5 {
		t.Fatalf("TopN len = %d, want 1..5", len(leg.TopN))
	}
}
