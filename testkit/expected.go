// Package testkit runs harness scenarios against ground truth and turns a
// simulation result into the expected impact numbers the engine is judged
// against. The exporter conformance suite every adapter must pass lives in
// the testkit/conformance subpackage.
//
// Evidence discipline mirrors the library's: Realized, Deferred, and the
// abandonment leg are exact (the omniscient ledger knows every amount);
// the suppressed-demand leg is an expectation (those transactions never
// drew amounts), computed from the generator's true distribution and
// labelled as such. The two are never summed here and must never be
// summed downstream.
package testkit

import (
	"time"

	"github.com/NightWatchEng/shortfall/examples/checkout"
)

// Expected holds a scenario's golden impact numbers over a window.
type Expected struct {
	Scenario string    `json:"scenario"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`

	// Realized: transactions that terminally failed inside the window.
	// Exact ledger truth.
	Realized struct {
		Count      int   `json:"count"`
		ValueMinor int64 `json:"value_minor"`
	} `json:"realized"`

	// Deferred: in-flight value at the snapshot instant To, by stage,
	// with the oldest age and capture-SLA breach count. Exact ledger
	// truth at the snapshot. Boundary contract the engine must replicate:
	// the snapshot includes the To instant (authed at exactly To counts,
	// age 0), while the window-scoped legs use [From, To) with To
	// excluded.
	Deferred struct {
		Count           int   `json:"count"`
		ValueMinor      int64 `json:"value_minor"`
		InCaptureCount  int   `json:"in_capture_count"`
		InSettleCount   int   `json:"in_settle_count"`
		OldestAgeMin    int   `json:"oldest_age_min"`
		CaptureBreaches int   `json:"capture_sla_breaches"`
	} `json:"deferred"`

	// Unrealized: what never happened. Abandonment is exact (the ledger
	// recorded the transactions telemetry never saw); suppressed demand
	// is an expectation — those arrivals never drew amounts, so their
	// value is NetLost x the generator's true mean amount.
	Unrealized struct {
		AbandonedCount      int   `json:"abandoned_count"`
		AbandonedValueMinor int64 `json:"abandoned_value_minor"`

		SuppressedCount       int   `json:"suppressed_count"`
		RecoveredCount        int   `json:"recovered_count"`
		NetLostCount          int   `json:"net_lost_count"`
		NetLostValueMinorEst  int64 `json:"net_lost_value_minor_est"`
		MeanAmountMinorForEst int64 `json:"mean_amount_minor_for_est"`
	} `json:"unrealized"`

	// Customers impacted: distinct customer ids across realized
	// transactions created in [From, To) and transactions in flight at
	// the To snapshot — not window-scoped for the deferred half, by the
	// same snapshot semantics as Deferred (a healthy pipeline contributes
	// its small baseline in-flight tail).
	Customers struct {
		Distinct int `json:"distinct"`
	} `json:"customers"`
}

// trueMeanAmountMinor is the exact expectation of the generator's amount
// distribution for a given enterprise fraction: amounts are uniform on
// [4200, 24199] (SMB) and [31000, 150999] (enterprise). Integer math
// throughout — the fraction is fixed to parts-per-million and the means
// are carried doubled so the .5s stay exact until the final division.
func trueMeanAmountMinor(enterpriseFraction float64) int64 {
	const (
		smbTwiceMean = 4200 + 24199   // 2 x 14199.5
		entTwiceMean = 31000 + 150999 // 2 x 90999.5
		million      = 1_000_000
	)
	ppm := int64(enterpriseFraction*million + 0.5)
	twiceMean := (smbTwiceMean*(million-ppm) + entTwiceMean*ppm) / million
	return twiceMean / 2
}

// ComputeExpected derives the golden numbers for a run over the given
// window. The deferred snapshot is taken at the window's To instant.
func ComputeExpected(name string, res checkout.Result, g checkout.GoldenWindow) Expected {
	var e Expected
	e.Scenario = name
	e.From, e.To = g.From, g.To
	snapshot := g.To

	inWindow := func(t time.Time) bool { return !t.Before(g.From) && t.Before(g.To) }

	customers := map[string]struct{}{}
	for _, txn := range res.Ledger.Txns {
		switch txn.State {
		case checkout.StateAuthFail, checkout.StateCapFail:
			if inWindow(txn.CreatedAt) {
				e.Realized.Count++
				e.Realized.ValueMinor += txn.AmountMinor
				customers[txn.CustomerID] = struct{}{}
			}
			continue
		case checkout.StateAbandoned:
			if inWindow(txn.CreatedAt) {
				e.Unrealized.AbandonedCount++
				e.Unrealized.AbandonedValueMinor += txn.AmountMinor
			}
			continue
		}

		// Deferred at the snapshot: authed by then, not yet terminal.
		if txn.AuthedAt.IsZero() || txn.AuthedAt.After(snapshot) {
			continue
		}
		inCapture := txn.CapturedAt.IsZero() || txn.CapturedAt.After(snapshot)
		inSettle := !inCapture && (txn.SettledAt.IsZero() || txn.SettledAt.After(snapshot))
		if !inCapture && !inSettle {
			continue // settled by the snapshot
		}
		e.Deferred.Count++
		e.Deferred.ValueMinor += txn.AmountMinor
		customers[txn.CustomerID] = struct{}{}
		var age time.Duration
		if inCapture {
			e.Deferred.InCaptureCount++
			age = snapshot.Sub(txn.AuthedAt)
			if age > g.CaptureSLA {
				e.Deferred.CaptureBreaches++
			}
		} else {
			e.Deferred.InSettleCount++
			age = snapshot.Sub(txn.CapturedAt)
		}
		if m := int(age.Minutes()); m > e.Deferred.OldestAgeMin {
			e.Deferred.OldestAgeMin = m
		}
	}

	for _, s := range res.Suppressed {
		if inWindow(s.Minute) {
			e.Unrealized.SuppressedCount += s.Count
		}
	}
	// Recoveries are counted by attribution: a recovered txn belongs to
	// the blackout that originally suppressed it (RecoveredFrom), so the
	// subtraction is window-coherent even with multiple blackouts in one
	// run — recoveries from other incidents never deflate this window's
	// NetLost, and re-suppressed demand is never double-counted.
	for _, txn := range res.Ledger.Txns {
		if txn.Recovered && inWindow(txn.RecoveredFrom) {
			e.Unrealized.RecoveredCount++
		}
	}
	e.Unrealized.NetLostCount = e.Unrealized.SuppressedCount - e.Unrealized.RecoveredCount
	if e.Unrealized.NetLostCount < 0 {
		// Unreachable under the attribution invariant; kept as a loud
		// guard rather than a silent clamp.
		panic("testkit: recovered exceeds suppressed for the same window — attribution invariant broken")
	}
	e.Unrealized.MeanAmountMinorForEst = trueMeanAmountMinor(res.Config.EnterpriseFraction)
	e.Unrealized.NetLostValueMinorEst = int64(e.Unrealized.NetLostCount) *
		e.Unrealized.MeanAmountMinorForEst

	e.Customers.Distinct = len(customers)
	return e
}
