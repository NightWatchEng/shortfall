// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/query"
)

func summaryFixture() engine.Report {
	from := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	return engine.Report{
		Request: engine.Request{
			Window: query.TimeRange{From: from, To: from.Add(time.Hour)},
			Flows:  []string{"invoice.pay"},
		},
		Realized: engine.Leg{
			Count: 2, ByCurrency: map[string]int64{"USD": 16999}, Evidence: engine.EvidenceDeterministic,
		},
		Deferred: engine.DeferredLeg{
			Leg: engine.Leg{ByCurrency: map[string]int64{"USD": 5000}, Evidence: engine.EvidenceDeterministic},
		},
		Unrealized: engine.EstLeg{
			LowMinor:  map[string]int64{"USD": 75522},
			MidMinor:  map[string]int64{"USD": 120000},
			HighMinor: map[string]int64{"USD": 164478},
			Evidence:  engine.EvidenceEstimate,
		},
		Customers: engine.CustomersLeg{
			Distinct: 3,
			TopN: []engine.CustomerImpact{
				{CustomerID: "h:c000002", Segment: "enterprise", ByCurrency: map[string]int64{"USD": 900000}},
				{CustomerID: "h:c000001", Segment: "smb", ByCurrency: map[string]int64{"USD": 14900, "EUR": 700}},
			},
		},
		Severity: "SEV2",
	}
}

// TestSummary pins the one-paragraph impact line the incident-tool writers
// PATCH into vendor fields: every leg keeps its own evidence tag, the
// counterfactual stays a range, and no figure ever merges realized with
// estimate.
func TestSummary(t *testing.T) {
	got := Summary(summaryFixture())
	want := "shortfall impact invoice.pay 2026-08-25T09:00Z→2026-08-25T10:00Z (minor units): " +
		"realized [deterministic] USD 16999 · deferred [deterministic] USD 5000 in-flight · " +
		"unrealized [estimate] USD 75522–164478 · customers 3 distinct · suggested SEV2"
	if got != want {
		t.Fatalf("summary =\n%q\nwant\n%q", got, want)
	}
}

// TestSummaryDegradedLegs pins honest degradation: an unavailable
// counterfactual or empty realized never fabricates a number.
func TestSummaryDegradedLegs(t *testing.T) {
	r := summaryFixture()
	r.Realized.ByCurrency = nil
	r.Unrealized = engine.EstLeg{Evidence: engine.EvidenceEstimate}
	r.Customers = engine.CustomersLeg{NotAvailableReason: "backend serves no events"}
	r.Severity = ""
	got := Summary(r)
	for _, must := range []string{"realized [deterministic] none", "unrealized [estimate] none", "customers n/a"} {
		if !strings.Contains(got, must) {
			t.Fatalf("summary missing %q: %q", must, got)
		}
	}
	if strings.Contains(got, "suggested") {
		t.Fatalf("no severity suggestion should render when empty: %q", got)
	}
}

// TestSummaryUnavailableLegsSayNA pins the ungrounded-leg contract: a leg
// Compute could not ground renders n/a in the vendor-field line, never a
// plausible-looking zero (the caveat naming why rides the JSON report).
func TestSummaryUnavailableLegsSayNA(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*engine.Report)
		want    string
		notWant string
	}{
		{
			"realized",
			func(r *engine.Report) {
				r.Realized = engine.Leg{Evidence: engine.EvidenceDeterministic, Caveats: []string{"unavailable: no event source"}, Unavailable: true}
			},
			"realized n/a", "realized [deterministic]",
		},
		{
			"deferred",
			func(r *engine.Report) {
				r.Deferred = engine.DeferredLeg{Leg: engine.Leg{Evidence: engine.EvidenceDeterministic, Caveats: []string{"unavailable: no metric source"}, Unavailable: true}}
			},
			"deferred n/a", "in-flight",
		},
		{
			"unrealized",
			func(r *engine.Report) {
				r.Unrealized = engine.EstLeg{Evidence: engine.EvidenceEstimate, Notes: []string{"unavailable: no metric source"}, Unavailable: true}
			},
			"unrealized n/a", "unrealized [estimate]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := summaryFixture()
			c.mutate(&r)
			got := Summary(r)
			if !strings.Contains(got, c.want) {
				t.Fatalf("summary missing %q: %q", c.want, got)
			}
			if strings.Contains(got, c.notWant) {
				t.Fatalf("summary must not render %q for an unavailable leg: %q", c.notWant, got)
			}
		})
	}
}

// TestMoneyRangeAsymmetricMaps pins the defensive rendering: Summary takes
// any Report, so bounds present in only some of the three maps must never
// render an inverted range, and a Mid-only leg is an estimate, not "none".
func TestMoneyRangeAsymmetricMaps(t *testing.T) {
	cases := []struct {
		name string
		leg  engine.EstLeg
		want string
	}{
		{
			name: "low only never inverts",
			leg:  engine.EstLeg{LowMinor: map[string]int64{"USD": 500}},
			want: "USD 500–500",
		},
		{
			name: "mid only is an estimate, not none",
			leg:  engine.EstLeg{MidMinor: map[string]int64{"USD": 120}},
			want: "USD 120–120",
		},
		{
			name: "full leg spans low to high",
			leg: engine.EstLeg{
				LowMinor:  map[string]int64{"USD": 100},
				MidMinor:  map[string]int64{"USD": 200},
				HighMinor: map[string]int64{"USD": 300},
			},
			want: "USD 100–300",
		},
		{
			name: "empty is none",
			leg:  engine.EstLeg{},
			want: "none",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := moneyRange(c.leg); got != c.want {
				t.Fatalf("moneyRange = %q, want %q", got, c.want)
			}
		})
	}
}
