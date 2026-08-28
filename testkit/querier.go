package testkit

import (
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query/memq"
)

// QuerierFromResult builds an in-memory query.Querier from a harness run's
// ground-truth ledger, so the engine can be exercised end to end with no
// backend process. Each TERMINAL transaction becomes one outcome event and
// its counter metric points (biz_txn_total, and biz_value_total carrying the
// exact amount); non-terminal transactions (still in flight at the snapshot)
// are the deferred leg's concern and are read from biz_inflight_value, which
// a deferred-leg test seeds directly.
//
// The mapping uses the flow name "invoice.pay" (the reference registry's
// flow) and the checkout lifecycle stages auth/capture/settle.
func QuerierFromResult(res checkout.Result) *memq.Querier {
	var events []biz.Outcome
	var metrics []emit.MetricPoint

	for _, txn := range res.Ledger.Txns {
		stage, result, at, terminal := terminalOf(txn)
		if !terminal {
			continue
		}
		vc := biz.ValueContext{
			Flow:       "invoice.pay",
			EntityID:   txn.ID,
			CustomerID: txn.CustomerID,
			Segment:    string(txn.Segment),
			Money:      biz.Money{Amount: txn.AmountMinor, Currency: txn.Currency, Exponent: 2},
			Kind:       biz.KindFee,
		}
		events = append(events, biz.Outcome{At: at, VC: vc, Stage: stage, Result: result, Source: "harness"})

		common := map[string]string{
			"flow": "invoice.pay", "stage": stage, "outcome": string(result),
			"currency": txn.Currency, "segment": string(txn.Segment),
		}
		metrics = append(metrics, emit.MetricPoint{Name: "biz_txn_total", Labels: common, Value: 1, At: at})

		valueLabels := map[string]string{
			"flow": "invoice.pay", "stage": stage, "outcome": string(result),
			"currency": txn.Currency, "kind": string(biz.KindFee), "segment": string(txn.Segment),
		}
		metrics = append(metrics, emit.MetricPoint{Name: "biz_value_total", Labels: valueLabels, Value: txn.AmountMinor, At: at})
	}

	return memq.New(memq.WithEvents(events), memq.WithMetrics(metrics))
}

// terminalOf maps a transaction's terminal state to (stage, result, time,
// terminal?). A non-terminal (still in-flight) transaction returns false.
// The event time is the most recent stage timestamp the state reached.
func terminalOf(txn checkout.Txn) (stage string, result biz.Result, at time.Time, terminal bool) {
	switch txn.State {
	case checkout.StateSettled:
		return "settle", biz.ResultSuccess, txn.SettledAt, true
	case checkout.StateCapFail:
		return "capture", biz.ResultFailed, firstNonZero(txn.CapturedAt, txn.AuthedAt, txn.CreatedAt), true
	case checkout.StateAuthFail:
		return "auth", biz.ResultFailed, firstNonZero(txn.AuthedAt, txn.CreatedAt), true
	case checkout.StateAbandoned:
		return "auth", biz.ResultAbandoned, firstNonZero(txn.AuthedAt, txn.CreatedAt), true
	default:
		// created / authed / captured — still in flight at the snapshot.
		return "", "", time.Time{}, false
	}
}

// firstNonZero returns the first non-zero time, or the last argument.
func firstNonZero(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	if len(times) == 0 {
		return time.Time{}
	}
	return times[len(times)-1]
}
