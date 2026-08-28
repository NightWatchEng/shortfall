package report

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/query"
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
// (15000) and the estimate legs are NEVER merged into one headline number.
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

	// Forbidden sums: realized + {mid, low, high} of the estimate.
	forbidden := []string{"20000", "17000", "23000"}
	for name, out := range renders {
		for _, sum := range forbidden {
			if strings.Contains(out, sum) {
				t.Fatalf("%s renderer contains %q — realized must never be summed with an estimate", name, sum)
			}
		}
		// Sanity: the separate figures ARE present.
		if !strings.Contains(out, "15000") {
			t.Fatalf("%s renderer missing the realized figure 15000", name)
		}
		if !strings.Contains(out, "5000") {
			t.Fatalf("%s renderer missing the estimate mid 5000", name)
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
