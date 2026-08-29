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
