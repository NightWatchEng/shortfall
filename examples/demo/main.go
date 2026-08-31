// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Command demo renders a real impact report with no backend running. It
// simulates a checkout system through an incident, feeds the ledger a real
// emitter could have seen into the in-memory querier, and computes the
// report the CLI would compute against Prometheus or SQL:
//
//	go run ./examples/demo
//
// The point is the evidence labels, not the amounts. The harness ledger is
// omniscient — it records abandoned transactions and suppressed demand that
// no telemetry could observe — and testkit.QuerierFromResult deliberately
// serves only the subset real telemetry would see. So the realized leg is
// deterministic, the unrealized leg is a range sized against the baseline,
// and the gap between them is the asymmetry the labels exist to carry.
//
// Coverage renders unavailable here, and would against a real backend too:
// engine.Compute never computes it. Coverage is a reconcile-time number that
// needs a provider ledger an impact request does not carry, so `shortfall
// reconcile` is what produces it. That is not a limitation of this demo.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/engine/report"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/NightWatchEng/shortfall/testkit"
)

// demoFlow is the flow the reference registry declares and the checkout
// harness emits under.
const demoFlow = "invoice.pay"

// demoRegistry is inline rather than a testdata file so the whole demo reads
// as one file. Its lookback_weeks drives how much history run simulates, so
// lowering it shortens the run and leaves the hour-of-week baseline with less
// to fit against — far enough and the unrealized leg degrades from a range to
// a point, which TestUnrealizedIsARangeNotAPoint fails on.
const demoRegistry = `
version: 1
segments: [smb, enterprise]
severity:
  - { sev: SEV1, min_per_minute: 100000 }
  - { sev: SEV2, min_per_minute: 10000 }
  - { sev: SEV3, min_per_minute: 1000 }
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
      - { name: settle,  signals: ["queue:settle.q"] }
    sla:
      capture: { deadline: PT30M, on_breach: lost }
    estimator: { default_minor: 18750, by_segment: { smb: 14200, enterprise: 91000 } }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 4 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.6, within: PT2H }
    reconcile: { source: "sql:ledger.payments" }
`

// Writer helpers, the same shape cmd/shortfall uses (impact.go): a demo has
// nowhere useful to report a failed write to, and errcheck is right that the
// return value cannot simply be ignored in silence.
func wf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func ws(w io.Writer, s string)                { _, _ = io.WriteString(w, s) }

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

func run(stdout, stderr io.Writer) int {
	reg, err := registry.Parse([]byte(demoRegistry))
	if err != nil {
		wf(stderr, "demo: registry: %v\n", err)
		return 1
	}

	// A Tuesday mid-morning incident, with the registry's full lookback of
	// the same weekday behind it so the baseline is fitted rather than
	// guessed. Read from the parsed registry rather than restated: a constant
	// here would be a second copy of demoRegistry's lookback_weeks, and the
	// day the two disagreed the estimate would quietly degrade to a point.
	flow, ok := reg.Flow(demoFlow)
	if !ok {
		wf(stderr, "demo: registry declares no flow %q\n", demoFlow)
		return 1
	}

	lookbackWeeks := flow.Baseline.LookbackWeeks
	incidentFrom := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	incidentTo := incidentFrom.Add(3 * time.Hour)
	histStart := incidentFrom.AddDate(0, 0, -7*lookbackWeeks)

	res := checkout.Run(checkout.Config{
		Seed:  901,
		Start: histStart,
		End:   incidentTo,
		Faults: []checkout.FaultSpec{
			{
				Kind: checkout.FaultAPI5xx,
				Rate: 0.35,
				From: incidentFrom.Add(10 * time.Minute),
				To:   incidentFrom.Add(65 * time.Minute),
			},
			{
				Kind: checkout.FaultAPILatency,
				Rate: 0.15,
				From: incidentFrom.Add(10 * time.Minute),
				To:   incidentFrom.Add(65 * time.Minute),
			},
		},
	})

	rep, err := engine.Compute(context.Background(), &reg, testkit.QuerierFromResult(res), engine.Request{
		Window: query.TimeRange{From: incidentFrom, To: incidentTo},
		Flows:  []string{demoFlow},
	})
	if err != nil {
		wf(stderr, "demo: compute: %v\n", err)
		return 1
	}

	return render(stdout, stderr, rep)
}

// render writes the report, or refuses when it would demonstrate nothing.
// Split out of run so the refusal can be tested directly: the harness is
// tuned to produce failures, so no input reachable from run exercises this
// branch, and a guard no test can enter is a guard nobody knows works.
func render(stdout, stderr io.Writer, rep engine.Report) int {
	// The faults in run are chosen to produce failures, so a realized leg of
	// nothing means the simulation or the query bridge stopped doing its job.
	// A demo that prints zeros convincingly is worse than one that fails.
	if rep.Realized.Count == 0 {
		ws(stderr, "demo: the realized leg is empty — the incident produced no "+
			"telemetry-visible failures, so this report would demonstrate nothing\n")
		return 1
	}

	ws(stdout, report.RenderText(rep))
	return 0
}
