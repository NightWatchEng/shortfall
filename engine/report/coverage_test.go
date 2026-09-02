// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NightWatchEng/shortfall/engine"
)

// twoSlices is the worked case throughout: telemetry saw USD 100.00 of the
// ledger's USD 200.00 on one slice and all of EUR 7.00 on another, so the
// headline is the WORST slice (50%), not the average (ADR-0011).
func twoSlices() (engine.CoverageLeg, []engine.CoverageSlice) {
	leg := engine.CoverageLeg{
		Evidence: engine.EvidenceTrust,
		Ratio:    0.5,
		Source:   "sql:ledger.payments",
	}
	// Deliberately out of order: every renderer must sort before printing.
	slices := []engine.CoverageSlice{
		{Flow: "invoice.pay", Currency: "EUR", Exponent: 2, TelemetryMinor: 700, LedgerMinor: 700, Ratio: 1},
		{Flow: "invoice.pay", Currency: "USD", Exponent: 2, TelemetryMinor: 10000, LedgerMinor: 20000, Ratio: 0.5},
		{Flow: "checkout.auth", Currency: "USD", Exponent: 2, TelemetryMinor: 100, LedgerMinor: 100, Ratio: 1},
	}
	return leg, slices
}

func TestRenderCoverageTextGrounded(t *testing.T) {
	leg, slices := twoSlices()
	got := RenderCoverageText(leg, slices)

	cases := []struct {
		name string
		want string
	}{
		{"headline names the worst ratio and the source", "COVERAGE   [trust] 50.0% reconciled against sql:ledger.payments"},
		{"slice attributes the telemetry side", "telemetry USD 100.00"},
		{"slice attributes the ledger side", "ledger USD 200.00"},
		{"every slice is listed, not just the worst", "EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(got, c.want) {
				t.Errorf("RenderCoverageText() missing %q, got:\n%s", c.want, got)
			}
		})
	}
}

func TestRenderCoverageSortsWithoutMutatingCaller(t *testing.T) {
	leg, slices := twoSlices()
	before := append([]engine.CoverageSlice(nil), slices...)

	cases := []struct {
		name   string
		render func()
	}{
		{"text", func() { RenderCoverageText(leg, slices) }},
		{"markdown", func() { RenderCoverageMarkdown(leg, slices) }},
		{"json", func() { _, _ = RenderCoverageJSON(leg, slices) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.render()
			for i := range before {
				if slices[i] != before[i] {
					t.Fatalf("renderer reordered the caller's slice at %d: got %+v, want %+v", i, slices[i], before[i])
				}
			}
		})
	}
}

func TestRenderCoverageOrdersSlicesDeterministically(t *testing.T) {
	leg, slices := twoSlices()

	cases := []struct {
		name  string
		got   string
		first string
		then  string
	}{
		{"text sorts by flow then currency", RenderCoverageText(leg, slices), "checkout.auth", "invoice.pay"},
		{"markdown sorts by flow then currency", RenderCoverageMarkdown(leg, slices), "checkout.auth", "invoice.pay"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i, j := strings.Index(c.got, c.first), strings.Index(c.got, c.then)
			if i < 0 || j < 0 || i > j {
				t.Errorf("want %q before %q, got:\n%s", c.first, c.then, c.got)
			}
		})
	}
}

func TestRenderCoverageUnavailableSaysWhy(t *testing.T) {
	leg := engine.CoverageLeg{
		Evidence:    engine.EvidenceTrust,
		Unavailable: "coverage needs a provider ledger — run shortfall reconcile for the trust number",
	}

	cases := []struct {
		name    string
		got     string
		wantSub string
		notWant string
	}{
		{"text names the reason", RenderCoverageText(leg, nil), "coverage needs a provider ledger", "0.0%"},
		{"markdown names the reason", RenderCoverageMarkdown(leg, nil), "coverage needs a provider ledger", "0.0%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(c.got, c.wantSub) {
				t.Errorf("missing reason %q, got:\n%s", c.wantSub, c.got)
			}

			// A zero here would be a claim: an ungrounded leg must never
			// render as a measured 0% (ADR-0017).
			if strings.Contains(c.got, c.notWant) {
				t.Errorf("ungrounded coverage rendered %q as a measured value:\n%s", c.notWant, c.got)
			}
		})
	}
}

func TestRenderCoverageJSONCarriesLegAndSlices(t *testing.T) {
	leg, slices := twoSlices()
	b, err := RenderCoverageJSON(leg, slices)
	if err != nil {
		t.Fatalf("RenderCoverageJSON: %v", err)
	}

	var got struct {
		Coverage engine.CoverageLeg     `json:"coverage"`
		Slices   []engine.CoverageSlice `json:"slices"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"ratio round-trips", got.Coverage.Ratio, 0.5},
		{"source round-trips", got.Coverage.Source, "sql:ledger.payments"},
		{"every slice round-trips", len(got.Slices), 3},
		{"slices are sorted", got.Slices[0].Flow, "checkout.auth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
}
