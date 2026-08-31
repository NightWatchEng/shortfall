// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// TestCounterfactualWithinInterval is the counterfactual leg's golden fence:
// for the upstream and api loci, the harness's ground-truth never-happened
// dollars (abandoned exactly, suppressed as the generator's expectation —
// never summed with realized anywhere) must fall inside the leg's
// [Low, High] estimate. Each run carries the registry's four weeks of
// fault-free history before a three-hour incident on the following Tuesday
// morning, so the hour-of-week baseline has a real basis. Both the leg and
// the ground truth derive from the same in-process run, so the assertion is
// platform-self-consistent (an interval check, not a byte golden).
func TestCounterfactualWithinInterval(t *testing.T) {
	reg, err := registry.Load("testdata/counterfactual-registry.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// Four weeks of lookback ending at the incident start, both Tuesdays.
	var (
		histStart    = time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
		incidentFrom = time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
		incidentTo   = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	)

	cases := []struct {
		name   string
		seed   uint64
		faults []checkout.FaultSpec
		// truth selects the ground-truth dollars the scenario creates.
		truth func(e Expected) int64
	}{
		{
			// The blackout opens half an hour before the measured window so
			// no pre-incident transaction settles inside it: with zero
			// in-window success events the AOV falls to the registry
			// estimator, which this test's registry pins to the generator's
			// true mean — the interval assertion then judges the baseline,
			// not a tiny biased AOV sample from boundary stragglers.
			name: "upstream blackout: suppressed demand within interval",
			seed: 407,
			faults: []checkout.FaultSpec{{
				Kind: checkout.FaultBlackout, From: incidentFrom.Add(-30 * time.Minute), To: incidentTo,
				RecoveredFraction: 0, RecoveryWithin: time.Hour,
			}},
			truth: func(e Expected) int64 { return e.Unrealized.NetLostValueMinorEst },
		},
		{
			name: "api latency + 5xx: abandonment within interval",
			seed: 901,
			faults: []checkout.FaultSpec{
				{Kind: checkout.FaultAPILatency, Rate: 0.30, From: incidentFrom, To: incidentTo},
				{Kind: checkout.FaultAPI5xx, Rate: 0.20, From: incidentFrom, To: incidentTo},
			},
			truth: func(e Expected) int64 { return e.Unrealized.AbandonedValueMinor },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := checkout.Run(checkout.Config{
				Seed:   c.seed,
				Start:  histStart,
				End:    incidentTo,
				Faults: c.faults,
			})
			window := query.TimeRange{From: incidentFrom, To: incidentTo}
			exp := ComputeExpected(c.name, res, checkout.GoldenWindow{
				From: incidentFrom, To: incidentTo, CaptureSLA: 30 * time.Minute,
			})
			truth := c.truth(exp)
			if truth == 0 {
				t.Fatal("scenario produced no ground-truth counterfactual dollars — the fence is vacuous")
			}

			leg, err := engine.Unrealized(context.Background(), &reg, QuerierFromResult(res), engine.Request{
				Window: window, Flows: []string{"invoice.pay"},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, note := range leg.Notes {
				if strings.Contains(note, "RETENTION GAP") || strings.Contains(note, "no baseline history") {
					t.Fatalf("baseline basis missing — the fence would be judging a degraded estimate: %q", note)
				}
			}
			low, mid, high := leg.LowMinor["USD"], leg.MidMinor["USD"], leg.HighMinor["USD"]
			if mid <= 0 {
				t.Fatalf("mid estimate is %d — the leg saw no shortfall (notes: %v)", mid, leg.Notes)
			}
			if truth < low || truth > high {
				t.Fatalf("ground truth %d outside [%d, %d] (mid %d, notes %v)", truth, low, high, mid, leg.Notes)
			}
		})
	}
}
