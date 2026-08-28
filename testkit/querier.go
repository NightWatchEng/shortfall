package testkit

import (
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query/memq"
)

// QuerierFromResult builds an in-memory query.Querier from a harness run's
// ground-truth ledger, modelling a TELEMETRY backend: it contains only what
// real telemetry could observe, so the engine can be exercised end to end
// with no backend process. Each telemetry-visible terminal transaction
// becomes one outcome event and its counter metric points (biz_txn_total,
// and biz_value_total carrying the exact amount).
//
// Deliberately excluded, because a real emitter never records them:
//   - abandoned transactions — the user gave up before the request landed;
//     "telemetry never saw it" (checkout.StateAbandoned). Abandoned rows stay
//     in res.Ledger as ground truth; this invisible loss is the
//     counterfactual leg's concern (baseline expected-vs-observed), never a
//     telemetry querier. (res.Suppressed separately holds blackout-suppressed
//     demand, a distinct invisible-loss source.)
//   - non-terminal (still in-flight) transactions — the deferred leg reads
//     these from biz_inflight_value, which a deferred-leg test seeds.
//
// The mapping uses the flow name "invoice.pay" (the reference registry's
// flow) and the checkout lifecycle stages auth/capture/settle.
func QuerierFromResult(res checkout.Result) *memq.Querier {
	return memq.New(memq.WithEvents(EventsFromResult(res)), memq.WithMetrics(MetricsFromResult(res)))
}

// EventsFromResult returns the telemetry-visible outcome events for a harness
// run — one per terminal, telemetry-visible transaction.
func EventsFromResult(res checkout.Result) []biz.Outcome {
	var events []biz.Outcome
	for _, txn := range res.Ledger.Txns {
		stage, result, at, visible := telemetryOutcome(txn)
		if !visible {
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
	}
	return events
}

// MetricsFromResult returns the metric points a real emitter would have shipped
// for a harness run: biz_txn_total + biz_value_total per telemetry-visible
// terminal transaction, plus the biz_inflight_value gauge snapshot at the run's
// end instant (res.Config.End) — the level a live scrape would publish "now".
// The golden harness (workspace-0ka) feeds these to BOTH memq and a real
// Prometheus so the two must return identical Series.
func MetricsFromResult(res checkout.Result) []emit.MetricPoint {
	return MetricsFromResultAt(res, res.Config.End)
}

// MetricsFromResultAt is MetricsFromResult with a caller-chosen instant for the
// in-flight gauge snapshot. The counters (biz_txn_total, biz_value_total) are
// stamped at their own event times; only the biz_inflight_value gauge is
// snapshotted at gaugeAt.
//
// The golden harness snapshots the gauge strictly INSIDE its query window: a
// gauge sample stamped exactly at the window end To is invisible to a half-open
// [From, To) read on BOTH sides — memq drops samples with At >= To and the
// promql adapter evaluates last_over_time at To-1ms — so an at-To snapshot would
// make the gauge parity assertion vacuous (empty == empty). A gaugeAt before To
// exercises the gauge/last_over_time translation for real.
func MetricsFromResultAt(res checkout.Result, gaugeAt time.Time) []emit.MetricPoint {
	var metrics []emit.MetricPoint
	for _, txn := range res.Ledger.Txns {
		stage, result, at, visible := telemetryOutcome(txn)
		if !visible {
			continue
		}
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
	// In-flight (deferred) value snapshot — the level the deferred leg reads.
	// Appended as biz_inflight_value gauge points stamped at gaugeAt.
	metrics = append(metrics, InFlightPointsAt(res, gaugeAt)...)
	return metrics
}

// InFlightPointsAt returns the biz_inflight_value gauge points for the demand
// still in a queue at instant `at` — transactions the run left in the capture
// queue (State authed) or the settle queue (State captured). Each point is the
// summed value for one (flow, stage, age_bucket, currency) at `at`; age is the
// time since the transaction entered that queue, bucketed per ADR-0005. This
// models what the InFlightTracker would have published at the snapshot.
func InFlightPointsAt(res checkout.Result, at time.Time) []emit.MetricPoint {
	// stage -> bucket -> currency -> value
	acc := map[string]map[string]map[string]int64{
		"capture": {},
		"settle":  {},
	}
	add := func(stage, bucket, currency string, v int64) {
		if acc[stage][bucket] == nil {
			acc[stage][bucket] = map[string]int64{}
		}
		acc[stage][bucket][currency] += v
	}
	for _, txn := range res.Ledger.Txns {
		switch txn.State {
		case checkout.StateAuthed: // waiting in the capture queue
			if !txn.AuthedAt.IsZero() && !txn.AuthedAt.After(at) {
				add("capture", emit.AgeBucketFor(at.Sub(txn.AuthedAt)), txn.Currency, txn.AmountMinor)
			}
		case checkout.StateCaptured: // waiting in the settle queue
			if !txn.CapturedAt.IsZero() && !txn.CapturedAt.After(at) {
				add("settle", emit.AgeBucketFor(at.Sub(txn.CapturedAt)), txn.Currency, txn.AmountMinor)
			}
		}
	}
	var points []emit.MetricPoint
	for stage, buckets := range acc {
		for bucket, byCur := range buckets {
			for currency, v := range byCur {
				points = append(points, emit.MetricPoint{
					Name: "biz_inflight_value",
					Labels: map[string]string{
						"flow": "invoice.pay", "stage": stage, "age_bucket": bucket, "currency": currency,
					},
					Value: v, At: at,
				})
			}
		}
	}
	return points
}

// telemetryOutcome maps a transaction's state to the outcome real telemetry
// would have recorded: (stage, result, time, visible?). A transaction that
// telemetry could not see — still in-flight, or abandoned before the request
// landed — returns visible=false. The event time is the most recent stage
// timestamp the state reached.
func telemetryOutcome(txn checkout.Txn) (stage string, result biz.Result, at time.Time, visible bool) {
	switch txn.State {
	case checkout.StateSettled:
		return "settle", biz.ResultSuccess, txn.SettledAt, true
	case checkout.StateCapFail:
		return "capture", biz.ResultFailed, firstNonZero(txn.CapturedAt, txn.AuthedAt, txn.CreatedAt), true
	case checkout.StateAuthFail:
		return "auth", biz.ResultFailed, firstNonZero(txn.AuthedAt, txn.CreatedAt), true
	default:
		// StateAbandoned: telemetry never saw it (counterfactual leg's
		// concern). created / authed / captured: still in flight at the
		// snapshot (deferred leg reads biz_inflight_value). Neither is a
		// telemetry-visible terminal outcome.
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
