// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
)

// QuerierFromResult builds an in-memory query.Querier from a harness run's
// ground-truth ledger, modelling a telemetry backend: it contains only what
// real telemetry could observe, so the engine can be exercised end to end
// with no backend process. Each telemetry-visible terminal transaction
// becomes one outcome event and its counter metric points (biz_txn_total,
// and biz_value_total carrying the exact amount); every transaction that
// entered the flow also counts once at the entry stage (see
// MetricsFromResultAt).
//
// Every transaction that enters a queue also records the `deferred`
// outcome there — at capture when authed, at settle when captured — the
// way the integration guide has a real emitter do, so the events-derived
// deferred path (ADR-0019) has the same basis as the gauge. Excluded,
// because a real emitter never records them: abandoned transactions
// (checkout.StateAbandoned) — telemetry never saw them; they stay in
// res.Ledger as ground truth for the counterfactual leg (res.Suppressed
// separately holds blackout-suppressed demand).
//
// The in-flight gauge points are what the real emit.InFlightTracker
// publishes when the ledger is replayed through it (InFlightPointsAt).
//
// The mapping uses the flow name "invoice.pay" (the reference registry's
// flow) and the checkout lifecycle stages auth/capture/settle.
func QuerierFromResult(res checkout.Result) *memq.Querier {
	return memq.New(memq.WithEvents(EventsFromResult(res)), memq.WithMetrics(MetricsFromResult(res)))
}

// EventsFromResult returns the telemetry-visible outcome events for a harness
// run: one `deferred` per queue entry (capture at AuthedAt, settle at
// CapturedAt) and one terminal event per terminal, telemetry-visible
// transaction.
func EventsFromResult(res checkout.Result) []biz.Outcome {
	var events []biz.Outcome
	for _, txn := range res.Ledger.Txns {
		vc := harnessVC(txn)
		for _, entry := range queueEntries(txn) {
			events = append(events, biz.Outcome{At: entry.at, VC: vc, Stage: entry.stage, Result: biz.ResultDeferred, Source: "harness"})
		}

		stage, result, at, visible := telemetryOutcome(txn)
		if !visible {
			continue
		}

		events = append(events, biz.Outcome{At: at, VC: vc, Stage: stage, Result: result, Source: "harness"})
	}

	return events
}

// harnessVC is the value context every event of a transaction carries.
func harnessVC(txn checkout.Txn) biz.ValueContext {
	return biz.ValueContext{
		Flow:       "invoice.pay",
		EntityID:   txn.ID,
		CustomerID: txn.CustomerID,
		Segment:    string(txn.Segment),
		Money:      biz.Money{Amount: txn.AmountMinor, Currency: txn.Currency, Exponent: 2},
		Kind:       biz.KindFee,
	}
}

// queueEntry is one moment a transaction entered a queued stage.
type queueEntry struct {
	stage string
	at    time.Time
}

// queueEntries lists the queued stages a transaction entered, in order: the
// capture queue at AuthedAt, the settle queue at CapturedAt. The harness
// never fails a capture, so a captured transaction always went on to the
// settle queue.
func queueEntries(txn checkout.Txn) []queueEntry {
	var entries []queueEntry
	if !txn.AuthedAt.IsZero() {
		entries = append(entries, queueEntry{"capture", txn.AuthedAt})
	}

	if !txn.CapturedAt.IsZero() {
		entries = append(entries, queueEntry{"settle", txn.CapturedAt})
	}

	return entries
}

// TrackerCadence is the publish interval the harness models for the
// in-flight gauge — the cadence the integration guide wires
// (`tr.Start(15 * time.Second)`).
const TrackerCadence = 15 * time.Second

// MetricsFromResult returns the metric points a real emitter would have shipped
// for a harness run: biz_txn_total + biz_value_total per telemetry-visible
// terminal transaction, an entry-stage biz_txn_total per transaction that
// entered the flow (see MetricsFromResultAt), plus the in-flight gauges as
// the tracker's last two publishes: one TrackerCadence before the run's end
// and one at the end instant (res.Config.End). A report over the half-open
// window [From, End) reads the earlier sample — the last level a live scrape
// would have seen — and a window that reaches past End reads the later one;
// a single sample stamped exactly at End would be invisible to the first and
// the deferred leg would fall back to events for a reason no real deployment
// has. The golden harness feeds these to both memq and a real Prometheus,
// which must return identical Series.
func MetricsFromResult(res checkout.Result) []emit.MetricPoint {
	end := res.Config.End
	metrics := metricsFromResultWithoutGauge(res)
	return append(metrics, inFlightPointsAtInstants(res, end.Add(-TrackerCadence), end)...)
}

// MetricsFromResultAt is MetricsFromResult with a caller-chosen instant for the
// in-flight gauge snapshot. The counters (biz_txn_total, biz_value_total) are
// stamped at their own event times; only the biz_inflight_value gauge is
// snapshotted at gaugeAt.
//
// Queue entries: each `deferred` outcome the emitter records at enqueue
// (see EventsFromResult) carries its biz_txn_total{outcome=deferred} point
// at the queued stage, as emit.Record does; no leg reads it, and the
// counter is here so the metric mirror matches the event stream.
//
// Entry-stage counts: every transaction telemetry saw enter the flow — auth
// succeeded, whatever happened later — additionally gets one
// biz_txn_total{stage=auth, outcome=success} point at its AuthedAt, so the
// counterfactual leg's entry basis (biz_txn_total at Stages[0], summed over
// outcomes) observes successful and in-flight transactions, not just
// terminal failures. Auth-failed transactions already count at the entry
// stage through their terminal point; abandoned ones stay invisible — that
// gap is the signal the counterfactual leg measures. No entry-stage
// biz_value_total point rides along: the harness emits success value only
// at the terminal stage, and the engine's coverage/AOV readers anchor at
// the flow's value stage (ADR-0016) — for the reference registry that is
// settle, so the terminal-only emission is exactly what they read.
//
// gaugeAt must lie strictly inside the query window: a sample stamped exactly
// at the window end To is invisible to a half-open [From, To) read on both
// sides (memq drops At >= To; the promql adapter reads last_over_time at
// To-1ms), which would make a gauge parity assertion vacuous (empty == empty).
func MetricsFromResultAt(res checkout.Result, gaugeAt time.Time) []emit.MetricPoint {
	return append(metricsFromResultWithoutGauge(res), InFlightPointsAt(res, gaugeAt)...)
}

// metricsFromResultWithoutGauge is the counter half of MetricsFromResultAt:
// every biz_txn_total and biz_value_total point, no in-flight gauge.
func metricsFromResultWithoutGauge(res checkout.Result) []emit.MetricPoint {
	var metrics []emit.MetricPoint
	for _, txn := range res.Ledger.Txns {
		for _, entry := range queueEntries(txn) {
			metrics = append(metrics, emit.MetricPoint{
				Name: "biz_txn_total",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": entry.stage, "outcome": string(biz.ResultDeferred),
					"currency": txn.Currency, "segment": string(txn.Segment),
				},
				Value: 1,
				At:    entry.at,
			})
		}

		if !txn.AuthedAt.IsZero() {
			metrics = append(metrics, emit.MetricPoint{
				Name: "biz_txn_total",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": "auth", "outcome": string(biz.ResultSuccess),
					"currency": txn.Currency, "segment": string(txn.Segment),
				},
				Value: 1,
				At:    txn.AuthedAt,
			})
		}

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
		metrics = append(metrics, emit.MetricPoint{
			Name:   "biz_value_total",
			Labels: valueLabels,
			Value:  txn.AmountMinor,
			At:     at,
		})
	}

	return metrics
}

// InFlightPointsAt returns the in-flight gauge points — biz_inflight_value
// and biz_inflight_count — for the demand still in a queue at instant `at`,
// exactly as the real emitter publishes them: the ledger is replayed through
// an emit.InFlightTracker (Track at each queue entry, Done at each exit, in
// time order up to `at`) feeding an emit.Std over the harness registry, and
// Publish runs once with the clock fixed at `at`. Age is the time since the
// transaction entered its queue, bucketed by the tracker (ADR-0005), and the
// count rides beside the value from one snapshot (ADR-0012). A transaction
// in flight at `at` that completed later is counted — the replay, unlike a
// read of the ledger's final state, sees the queue as it was then.
//
// A replay failure panics: the harness registry and emitter are constants,
// so a failure here is a bug in this file, not a scenario.
func InFlightPointsAt(res checkout.Result, at time.Time) []emit.MetricPoint {
	return inFlightPointsAtInstants(res, at)
}

// queueEvent is one Track or Done the replay feeds the tracker. Events are
// appended per transaction in lifecycle order (Track capture, Done capture,
// Track settle, Done settle) and sorted STABLY by time alone, so a
// transaction whose stages share an instant keeps its own order — its Done
// never lands before its Track — while events of different transactions at
// one instant, which touch different ids, may interleave freely.
type queueEvent struct {
	at    time.Time
	apply func(*emit.InFlightTracker)
}

// inFlightPointsAtInstants replays the ledger through ONE tracker and
// publishes at each instant in ascending order, so a combo that drains
// between two instants is zeroed by the tracker's own retire pass at the
// later one — the sample a live tracker would have published — rather than
// silently absent, which a last-level read would carry forward as stale.
func inFlightPointsAtInstants(res checkout.Result, instants ...time.Time) []emit.MetricPoint {
	reg, err := registry.Parse([]byte(harnessRegistry))
	if err != nil {
		panic(fmt.Sprintf("testkit: harness registry: %v", err))
	}

	now := instants[0]
	clock := func() time.Time { return now }
	exp := &capturingExporter{}
	em, err := emit.New(&reg, exp, emit.WithFlushInterval(0), emit.WithClock(clock))
	if err != nil {
		panic(fmt.Sprintf("testkit: harness emitter: %v", err))
	}

	tr := emit.NewInFlightTracker(em, emit.WithTrackerClock(clock))

	var timeline []queueEvent
	for _, txn := range res.Ledger.Txns {
		txn := txn
		money := biz.Money{Amount: txn.AmountMinor, Currency: txn.Currency, Exponent: 2}
		if !txn.AuthedAt.IsZero() {
			timeline = append(timeline, queueEvent{txn.AuthedAt, func(t *emit.InFlightTracker) {
				t.Track("invoice.pay", "capture", txn.ID, money, txn.AuthedAt)
			}})
		}

		if !txn.CapturedAt.IsZero() {
			timeline = append(timeline, queueEvent{txn.CapturedAt, func(t *emit.InFlightTracker) {
				t.Done("invoice.pay", "capture", txn.ID)
			}})
			timeline = append(timeline, queueEvent{txn.CapturedAt, func(t *emit.InFlightTracker) {
				t.Track("invoice.pay", "settle", txn.ID, money, txn.CapturedAt)
			}})
		}

		if !txn.SettledAt.IsZero() {
			timeline = append(timeline, queueEvent{txn.SettledAt, func(t *emit.InFlightTracker) {
				t.Done("invoice.pay", "settle", txn.ID)
			}})
		}
	}

	sort.SliceStable(timeline, func(i, j int) bool { return timeline[i].at.Before(timeline[j].at) })

	sorted := append([]time.Time(nil), instants...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })

	next := 0
	for _, at := range sorted {
		for next < len(timeline) && !timeline[next].at.After(at) {
			timeline[next].apply(tr)
			next++
		}

		now = at
		tr.Publish()
		if err := em.Flush(context.Background()); err != nil {
			panic(fmt.Sprintf("testkit: harness emitter flush: %v", err))
		}
	}

	if err := em.Close(context.Background()); err != nil {
		panic(fmt.Sprintf("testkit: harness emitter close: %v", err))
	}

	if tr.Overflowed() != 0 || tr.Rejected() != 0 {
		panic(fmt.Sprintf("testkit: tracker replay rejected %d and overflowed %d Track calls", tr.Rejected(), tr.Overflowed()))
	}

	return exp.points
}

// harnessRegistry declares the one flow the checkout harness emits under,
// with the reference registry's stages, so the emitter's label fence admits
// every point the replay publishes.
const harnessRegistry = `
version: 1
segments: [smb, enterprise]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
      - { name: settle,  signals: ["queue:settle.q"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "sql:ledger.payments" }
`

// capturingExporter keeps every metric point the emitter flushes. The
// replay emits no events, so the events half is an honest no-op.
type capturingExporter struct{ points []emit.MetricPoint }

func (c *capturingExporter) ExportMetrics(_ context.Context, batch []emit.MetricPoint) error {
	c.points = append(c.points, batch...)
	return nil
}

func (c *capturingExporter) ExportEvents(context.Context, []biz.Outcome) error { return nil }
func (c *capturingExporter) Capabilities() emit.Caps                           { return emit.Caps{Metrics: true} }
func (c *capturingExporter) Shutdown(context.Context) error                    { return nil }

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
