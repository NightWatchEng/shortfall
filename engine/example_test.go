// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"fmt"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
)

// Compute answers an incident window with the four legs, each labelled by
// its evidence. The querier here is the in-memory reference; production
// uses a backend adapter (adapters/query/promql, adapters/query/sql, ...).
func ExampleCompute() {
	reg, err := registry.Parse([]byte(`
version: 1
segments: [smb]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 4 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.0 }
    reconcile: { source: "sql:ledger.payments" }
`))
	if err != nil {
		panic(err)
	}

	// Two capture failures for the same invoice (a retry): the realized leg
	// de-duplicates by entity, so the loss counts once.
	from := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	fail := func(entity string, amount int64, at time.Time) biz.Outcome {
		return biz.Outcome{
			At: at, Stage: "capture", Result: biz.ResultFailed,
			VC: biz.ValueContext{
				Flow: "invoice.pay", EntityID: entity, CustomerID: "h:c000007",
				Segment: "smb", Kind: biz.KindFee,
				Money: biz.Money{Amount: amount, Currency: "USD", Exponent: 2},
			},
		}
	}
	q := memq.New(memq.WithEvents([]biz.Outcome{
		fail("inv_00000042", 4999, from.Add(5*time.Minute)),
		fail("inv_00000042", 4999, from.Add(9*time.Minute)), // retry, de-duped
		fail("inv_00000099", 12000, from.Add(20*time.Minute)),
	}))

	report, err := engine.Compute(context.Background(), &reg, q, engine.Request{
		Window: query.TimeRange{From: from, To: from.Add(time.Hour)},
		Flows:  []string{"invoice.pay"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("realized: %d txns, USD minor %d (%s)\n",
		report.Realized.Count, report.Realized.ByCurrency["USD"], report.Realized.Evidence)
	// Output: realized: 2 txns, USD minor 16999 (deterministic)
}
