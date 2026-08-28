package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/NightWatchEng/shortfall/engine/baseline"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// Unrealized estimates the counterfactual leg (M7): demand the incident
// suppressed or drove to abandonment — value that never became a
// telemetry-visible outcome, so no deterministic leg can see it. For each flow
// and currency it is, summed over the incident hours:
//
//	max(0, expected_entries - observed_entries) * AOV * (1 - recovered_fraction)
//
// where expected_entries is the baseline (ADR-0006 hour-of-week median, with its
// interval) and observed_entries is biz_txn_total at the flow's ENTRY stage
// (registry Stages[0], summed over outcomes) — every transaction that reached
// the flow is counted once there, so suppressed/abandoned demand shows up as the
// shortfall. AOV is the observed captured average (biz_value_total / biz_txn_total
// for successes) in that currency, falling back to the registry estimator.
//
// It is reported ONLY as a range: the baseline's Lower/Upper expectations give
// the Low/High unrealized bounds, Mid uses the median. Evidence is always
// estimate, and the leg is NEVER added to realized loss.
//
// entries basis (v0, workspace-tmw.8.2): counting at the entry stage assumes the
// emitter records biz_txn_total there for every transaction. The distinct-entity
// alternative is model-independent but query-heavy; see the bead.
func Unrealized(ctx context.Context, reg *registry.Registry, q query.Querier, req Request) (EstLeg, error) {
	leg := EstLeg{
		LowMinor:  map[string]int64{},
		MidMinor:  map[string]int64{},
		HighMinor: map[string]int64{},
		Evidence:  EvidenceEstimate,
	}
	if reg == nil {
		leg.Notes = []string{"unavailable: the counterfactual leg needs a registry (baseline lookback, stages, recovery)"}
		return leg, nil
	}
	if !q.Capabilities().Metrics {
		leg.Notes = []string{"unavailable: the counterfactual leg needs a metric source for stage-entry history"}
		return leg, nil
	}

	// The incident hours we estimate over, and the lookback window feeding the
	// baseline. Both are hour-aligned; the baseline enforces the lookback bound.
	target := hourlyInstants(req.Window)
	if len(target) == 0 {
		leg.Notes = []string{"window shorter than one hour — no counterfactual estimate"}
		return leg, nil
	}

	flows := req.Flows
	if len(flows) == 0 {
		leg.Notes = append(leg.Notes, "no flow named in the request — the counterfactual leg needs per-flow baseline configuration")
		return leg, nil
	}

	var notes []string
	thin := false
	for _, flowName := range flows {
		flow, ok := reg.Flow(flowName)
		if !ok || len(flow.Stages) == 0 {
			notes = append(notes, fmt.Sprintf("flow %q not in the registry (or has no stages) — skipped", flowName))
			continue
		}
		if flow.Baseline.LookbackWeeks < 1 {
			notes = append(notes, fmt.Sprintf("flow %q has no baseline lookback — skipped", flowName))
			continue
		}
		// Retention gap (workspace-tmw.8.3): if the querier declares less metric
		// history than the baseline lookback needs, do NOT silently compute a
		// baseline from too little data — flag the gap and suggest a
		// longer-retention source. A querier that declares 0 weeks means
		// "unknown/undeclared" (not "zero"), so it is not treated as a gap.
		if hw := q.Capabilities().MetricHistoryWeeks; hw > 0 && hw < flow.Baseline.LookbackWeeks {
			notes = append(notes, fmt.Sprintf(
				"flow %q: RETENTION GAP — the querier serves %d week(s) of metric history but the baseline needs %d; not estimated, to avoid a counterfactual built from too little history. Point the counterfactual at a longer-retention (e.g. warehouse) querier.",
				flowName, hw, flow.Baseline.LookbackWeeks))
			continue
		}
		if flow.Baseline.Holidays != "" {
			notes = append(notes, fmt.Sprintf("flow %q declares holiday calendar %q, which v0 does not yet apply", flowName, flow.Baseline.Holidays))
		}
		entryStage := flow.Stages[0].Name

		// Both history and observed are bucketed on the SAME hour-aligned grid as
		// target — memq/adapters bucket from Range.From, so querying observed over
		// req.Window (which need not be hour-aligned) would offset the buckets and
		// pair each target hour with the wrong observed count. Query the aligned
		// span [target[0], lastTarget+1h) instead.
		alignedWindow := query.TimeRange{From: target[0], To: target[len(target)-1].Add(time.Hour)}
		hist, err := hourlyEntriesByCurrency(ctx, q, flowName, entryStage,
			query.TimeRange{From: target[0].AddDate(0, 0, -7*flow.Baseline.LookbackWeeks), To: target[0]})
		if err != nil {
			return EstLeg{}, fmt.Errorf("engine: unrealized history query: %w", err)
		}
		obs, err := hourlyEntriesByCurrency(ctx, q, flowName, entryStage, alignedWindow)
		if err != nil {
			return EstLeg{}, fmt.Errorf("engine: unrealized observed query: %w", err)
		}

		for currency, histSamples := range hist {
			exp, err := (baseline.HourOfWeek{}).Expected(histSamples, target, baseline.Config{LookbackWeeks: flow.Baseline.LookbackWeeks})
			if err != nil {
				return EstLeg{}, fmt.Errorf("engine: unrealized baseline: %w", err)
			}
			aov, aovSource, warn, ok := aovMinor(ctx, q, flowName, currency, flow, req.Window)
			if warn != "" {
				notes = append(notes, warn)
			}
			if !ok {
				notes = append(notes, fmt.Sprintf("flow %q currency %s: no observed AOV and no applicable registry estimator — not valued", flowName, currency))
				continue
			}
			if aovSource == "metric" && flow.Estimator != nil {
				notes = append(notes, fmt.Sprintf("flow %q currency %s: AOV from the value counter may understate — this flow emits estimated successes the counter omits, and no event source was available to correct it", flowName, currency))
			}
			recovery := clampFraction(flow.Recovery.RecoveredFraction)
			observedAt := hourMap(obs[currency])
			var low, mid, high float64
			for i, e := range exp {
				if e.N == 0 {
					thin = true
					continue // no history for this hour-of-week; 8.3 flags the gap
				}
				o := observedAt[hourKey(target[i])]
				low += shortfallValue(e.Lower, o, aov, recovery)
				mid += shortfallValue(e.Expected, o, aov, recovery)
				high += shortfallValue(e.Upper, o, aov, recovery)
			}
			leg.LowMinor[currency] += int64(math.Round(low))
			leg.MidMinor[currency] += int64(math.Round(mid))
			leg.HighMinor[currency] += int64(math.Round(high))
		}

		if r := clampFraction(flow.Recovery.RecoveredFraction); r > 0 {
			notes = append(notes, fmt.Sprintf("flow %q: net of an assumed %.0f%% recovery of suppressed demand", flowName, r*100))
		}
	}

	if thin {
		notes = append(notes, "some incident hours had no baseline history (retention gap) and were excluded — see the coverage/retention note")
	}
	if hint := upstreamAttribution(ctx, q, req); hint != "" {
		notes = append(notes, hint)
	}
	leg.Notes = notes
	return leg, nil
}

// shortfallValue is one hour's unrealized value: the suppressed entries
// (expected minus observed, never negative) valued at AOV, net of recovery.
func shortfallValue(expected, observed float64, aovMinorUnits int64, recovery float64) float64 {
	short := expected - observed
	if short <= 0 {
		return 0
	}
	return short * float64(aovMinorUnits) * (1 - recovery)
}

// hourlyEntriesByCurrency returns, per currency, the hourly stage-entry counts
// (biz_txn_total at entryStage, summed over outcomes) as baseline samples.
func hourlyEntriesByCurrency(ctx context.Context, q query.Querier, flow, entryStage string, r query.TimeRange) (map[string][]baseline.Sample, error) {
	series, err := q.QueryMetric(ctx, query.Query{
		Metric:  "biz_txn_total",
		Agg:     query.AggSum,
		Filters: map[string]string{"flow": flow, "stage": entryStage},
		GroupBy: []string{"currency"},
		Range:   r,
		Step:    time.Hour,
	})
	if err != nil {
		return nil, err
	}
	out := map[string][]baseline.Sample{}
	for _, s := range series {
		cur := s.Labels["currency"]
		for _, p := range s.Points {
			out[cur] = append(out[cur], baseline.Sample{At: p.At, Count: p.Value})
		}
	}
	return out, nil
}

// aovMinor is the observed average order value in minor units for a currency,
// with a source label for the notes. Preference order:
//  1. EVENTS (exact source of truth): sum of success amounts / count of success
//     events. This INCLUDES estimated amounts — they ride the event — so it is
//     unbiased where the biz_value_total counter, which omits estimated
//     successes, would understate.
//  2. the biz_value_total/biz_txn_total metric ratio (only when events are not
//     served); flagged as possibly understated for flows with estimated traffic.
//  3. the registry estimator, applied ONLY for a currency the flow DECLARES
//     (the estimator has no intrinsic currency, so it is not stamped onto a
//     currency a declared-currency flow did not list). Its amount is used as
//     minor units; the registry is responsible for declaring the estimator at
//     the currency's exponent (Finance-reviewed), biz_value_total's basis.
//
// ok is false when none is available (the currency is then left unvalued). warn
// is a non-empty disclosure the caller must surface (e.g. an events-backend
// failure that silently degraded AOV to the counter).
func aovMinor(ctx context.Context, q query.Querier, flow, currency string, f registry.Flow, window query.TimeRange) (aov int64, source, warn string, ok bool) {
	if q.Capabilities().Events {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   window,
			Filters: map[string]string{"flow": flow, "outcome": "success", "currency": currency},
		})
		switch {
		case err != nil:
			// A real events-backend failure, not "no events". Disclose it — the
			// counter fallback below omits estimated successes and can understate.
			warn = fmt.Sprintf("flow %q currency %s: AOV events query failed (%v); fell back to the value counter", flow, currency, err)
		default:
			var sum, count int64
			for _, g := range groups {
				sum += g.SumMinor
				count += g.Count
			}
			if count > 0 {
				return int64(math.Round(float64(sum) / float64(count))), "events", warn, true
			}
		}
	}
	if q.Capabilities().Metrics {
		filters := map[string]string{"flow": flow, "outcome": "success", "currency": currency}
		value := sumMetric(ctx, q, "biz_value_total", filters, window)
		count := sumMetric(ctx, q, "biz_txn_total", filters, window)
		if count > 0 && value > 0 {
			return int64(math.Round(value / count)), "metric", warn, true
		}
	}
	// The estimator has no intrinsic currency — EstimateMoney stamps whatever
	// currency is asked for — so it is trustworthy only for a currency the flow
	// actually declares. For an undeclared flow (empty Currencies) we cannot know
	// the estimator's basis, so it is applied only when the flow's currency set
	// is unspecified (the registry's own choice), never onto a currency a
	// declared-currency flow did not list.
	if m, ok := f.EstimateMoney("", currency); ok && flowAllowsCurrency(f, currency) {
		return m.Amount, "estimator", warn, true
	}
	return 0, "", warn, false
}

// flowAllowsCurrency reports whether currency is one the flow declares (or the
// flow declares none, i.e. any is accepted per ADR — Currencies empty).
func flowAllowsCurrency(f registry.Flow, currency string) bool {
	if len(f.Currencies) == 0 {
		return true
	}
	for _, c := range f.Currencies {
		if c == currency {
			return true
		}
	}
	return false
}

// sumMetric is the scalar sum of a counter metric over the window (Step 0).
func sumMetric(ctx context.Context, q query.Querier, metric string, filters map[string]string, window query.TimeRange) float64 {
	series, err := q.QueryMetric(ctx, query.Query{Metric: metric, Agg: query.AggSum, Filters: filters, Range: window})
	if err != nil {
		return 0
	}
	var total float64
	for _, s := range series {
		for _, p := range s.Points {
			total += p.Value
		}
	}
	return total
}

// upstreamAttribution turns provider-call telemetry into a one-line hint: if the
// wrapped client recorded failed provider calls in the window, the suppression
// may be upstream. Absent that metric it returns "".
func upstreamAttribution(ctx context.Context, q query.Querier, req Request) string {
	failed := sumMetric(ctx, q, "biz_provider_calls_total", map[string]string{"outcome": "failed"}, req.Window)
	if failed <= 0 {
		return ""
	}
	return fmt.Sprintf("attribution hint: %d failed provider call(s) in the window — suppression may be upstream", int64(failed))
}

// hourlyInstants returns the hour-aligned start of every hour that begins in
// [window.From, window.To).
func hourlyInstants(w query.TimeRange) []time.Time {
	start := w.From.Truncate(time.Hour)
	if start.Before(w.From) {
		start = start.Add(time.Hour)
	}
	var out []time.Time
	for t := start; t.Before(w.To); t = t.Add(time.Hour) {
		out = append(out, t)
	}
	return out
}

// hourMap indexes a currency's observed samples by their hour, so each target
// hour can be matched to what was actually observed then.
func hourMap(samples []baseline.Sample) map[int64]float64 {
	m := make(map[int64]float64, len(samples))
	for _, s := range samples {
		m[hourKey(s.At)] += s.Count
	}
	return m
}

// hourKey maps observed hourly points to the target hour they fall in.
func hourKey(t time.Time) int64 { return t.Truncate(time.Hour).Unix() }

// clampFraction bounds a fraction to [0, 1]; a misconfigured recovery cannot
// inflate loss (negative) or erase it beyond 100%.
func clampFraction(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
