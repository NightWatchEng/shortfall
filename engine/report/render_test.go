package report

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
)

var update = flag.Bool("update", false, "update golden files")

// sampleReport builds a representative report with all evidence tags and a
// populated set of legs. Values are chosen so that realized (15000) and the
// estimate mid (5000) have a distinct sum (20000) that must appear in NO
// renderer.
func sampleReport() engine.Report {
	win := query.TimeRange{
		From: time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC),
	}
	return engine.Report{
		Request:         engine.Request{Window: win, Flows: []string{"invoice.pay"}},
		GeneratedAt:     time.Date(2026, 8, 27, 16, 1, 0, 0, time.UTC),
		RegistryVersion: 1,
		LibraryVersion:  "v0.1.0",
		Realized: engine.Leg{
			Count: 3, ByCurrency: map[string]int64{"USD": 15000}, Evidence: engine.EvidenceDeterministic,
		},
		Deferred: engine.DeferredLeg{
			Leg:                engine.Leg{ByCurrency: map[string]int64{"USD": 5568661}, Evidence: engine.EvidenceDeterministic},
			ByAgeBucket:        map[string]map[string]int64{"gt2h": {"USD": 5568661}},
			OldestAgeMinutes:   120,
			ProjectedLostMinor: map[string]int64{"USD": 500},
		},
		Unrealized: engine.EstLeg{
			LowMinor: map[string]int64{"USD": 2000}, MidMinor: map[string]int64{"USD": 5000}, HighMinor: map[string]int64{"USD": 8000},
			Evidence: engine.EvidenceEstimate, Notes: []string{"assumes 60% recovery within 2h"},
		},
		Customers: engine.CustomersLeg{
			Distinct: 2, BySegment: map[string]int64{"smb": 1, "enterprise": 1},
			TopN: []engine.CustomerImpact{
				{CustomerID: "h:c2", Segment: "enterprise", ByCurrency: map[string]int64{"USD": 9000}},
				{CustomerID: "h:c1", Segment: "smb", ByCurrency: map[string]int64{"USD": 6000}},
			},
		},
		Coverage: engine.CoverageLeg{Ratio: 0.98, Source: "sql:ledger.payments", Evidence: engine.EvidenceTrust, Window: win},
		Severity: "SEV2",
	}
}

func TestGoldenRenders(t *testing.T) {
	r := sampleReport()
	cases := []struct {
		name   string
		file   string
		render func() ([]byte, error)
	}{
		{"json", "report.json", func() ([]byte, error) { return RenderJSON(r) }},
		{"text", "report.txt", func() ([]byte, error) { return []byte(RenderText(r)), nil }},
		{"markdown", "report.md", func() ([]byte, error) { return []byte(RenderMarkdown(r)), nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.render()
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", c.file)
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s mismatch.\n--- got ---\n%s\n--- want ---\n%s", c.name, got, want)
			}
		})
	}
}

// TestNoRendererSumsRealizedWithEstimate is the rendering invariant: realized
// (15000) and the estimate legs are never merged into one headline number.
func TestNoRendererSumsRealizedWithEstimate(t *testing.T) {
	r := sampleReport()
	renders := map[string]string{
		"text":     RenderText(r),
		"markdown": RenderMarkdown(r),
	}
	j, err := RenderJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	renders["json"] = string(j)

	// Forbidden sums derived FROM the fixture (not hardcoded), so the guard
	// tracks the sample: realized + each of the estimate low/mid/high.
	rz := r.Realized.ByCurrency["USD"]
	forbidden := []string{
		fmt.Sprint(rz + r.Unrealized.LowMinor["USD"]),
		fmt.Sprint(rz + r.Unrealized.MidMinor["USD"]),
		fmt.Sprint(rz + r.Unrealized.HighMinor["USD"]),
	}
	for name, out := range renders {
		for _, sum := range forbidden {
			if strings.Contains(out, sum) {
				t.Fatalf("%s renderer contains %q — realized must never be summed with an estimate", name, sum)
			}
		}
		// Sanity: the separate figures are present. Use the realized value and
		// the estimate low/high (2000/8000) — values that are not substrings
		// of any other rendered number, so a dropped estimate leg is caught
		// (the mid 5000 would be a substring of realized 15000 and prove
		// nothing).
		if !strings.Contains(out, "15000") {
			t.Fatalf("%s renderer missing the realized figure 15000", name)
		}
		if !strings.Contains(out, "2000") || !strings.Contains(out, "8000") {
			t.Fatalf("%s renderer missing the estimate range 2000…8000", name)
		}
	}
}

func TestEvidenceTagsPresent(t *testing.T) {
	r := sampleReport()
	text := RenderText(r)
	for _, tag := range []string{"deterministic", "estimate", "trust"} {
		if !strings.Contains(text, tag) {
			t.Fatalf("text render missing evidence tag %q", tag)
		}
	}
}

// TestComputedEventsOnlyReportStatesDeferredGap drives engine.Compute over
// an events-only backend (the shape the sql/logql/cwinsights/spl queriers
// serve) and asserts the deferred leg's metrics-unavailable caveat survives
// into both human renders — the exact plausible-looking zero this package
// used to print.
func TestComputedEventsOnlyReportStatesDeferredGap(t *testing.T) {
	reg, err := registry.Load("../../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	q := memq.New(memq.WithCaps(query.Caps{Events: true, EventHistoryWeeks: 520}))
	rep, err := engine.Compute(context.Background(), &reg, q, engine.Request{
		Window: query.TimeRange{
			From: time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC),
		},
		Flows: []string{"invoice.pay"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"text": RenderText(rep), "markdown": RenderMarkdown(rep)} {
		if !strings.Contains(out, "biz_inflight_value") {
			t.Fatalf("%s render of an events-only report must state the deferred leg's missing metric source, got:\n%s", name, out)
		}
	}
	if s := Summary(rep); !strings.Contains(s, "deferred n/a") {
		t.Fatalf("summary of an events-only report must say the deferred leg is n/a, got: %q", s)
	}
}

// TestUngroundedLegLabelsRendered is the honesty invariant for degraded
// reports: every label engine.Compute attaches when a leg cannot be
// grounded (Caveats, Notes, NotAvailableReason, Unavailable) must appear
// in BOTH human renders — a dropped label turns an ungrounded leg into a
// plausible-looking zero.
func TestUngroundedLegLabelsRendered(t *testing.T) {
	r := sampleReport()
	r.Realized.Caveats = []string{"metrics-only: upper bound, not de-duped by entity"}
	r.Deferred.Caveats = []string{"unavailable: deferred leg needs a metric source (biz_inflight_value)"}
	r.Unrealized.Notes = []string{"unavailable: unrealized leg needs a metric source"}
	r.Customers = engine.CustomersLeg{NotAvailableReason: "backend serves no events"}
	r.Coverage = engine.CoverageLeg{Unavailable: "no reconciliation has run"}

	labels := []string{
		"metrics-only: upper bound, not de-duped by entity",
		"unavailable: deferred leg needs a metric source (biz_inflight_value)",
		"unavailable: unrealized leg needs a metric source",
		"backend serves no events",
		"no reconciliation has run",
	}
	cases := []struct {
		name   string
		render func(engine.Report) string
	}{
		{"text", RenderText},
		{"markdown", RenderMarkdown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := c.render(r)
			for _, label := range labels {
				if !strings.Contains(out, label) {
					t.Errorf("render drops the ungrounded-leg label %q", label)
				}
			}
		})
	}
}

func TestNotAvailableAndUnavailableRendered(t *testing.T) {
	r := sampleReport()
	r.Customers = engine.CustomersLeg{NotAvailableReason: "backend serves no events"}
	r.Coverage = engine.CoverageLeg{Unavailable: "no reconciliation has run"}
	for name, out := range map[string]string{"text": RenderText(r), "markdown": RenderMarkdown(r)} {
		if !strings.Contains(out, "backend serves no events") {
			t.Fatalf("%s must render the customers NotAvailableReason", name)
		}
		if !strings.Contains(out, "no reconciliation has run") {
			t.Fatalf("%s must render the coverage Unavailable reason", name)
		}
	}
}
