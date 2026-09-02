// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/engine"
)

// CoverageReport pairs the trust headline with its per-slice attribution.
// The JSON rendering carries both for the same reason the text block prints
// both: a sub-100% ratio is only actionable once a (flow, currency) slice
// names where telemetry and the ledger diverged.
type CoverageReport struct {
	Coverage engine.CoverageLeg     `json:"coverage"`
	Slices   []engine.CoverageSlice `json:"slices"`
}

// sortedSlices returns the slices in (flow, currency) order without touching
// the caller's backing array — a renderer that reorders its input makes the
// order depend on which format ran first.
func sortedSlices(slices []engine.CoverageSlice) []engine.CoverageSlice {
	out := append([]engine.CoverageSlice(nil), slices...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Flow != out[j].Flow {
			return out[i].Flow < out[j].Flow
		}

		return out[i].Currency < out[j].Currency
	})

	return out
}

// RenderCoverageJSON renders the trust line as machine-readable JSON.
func RenderCoverageJSON(leg engine.CoverageLeg, slices []engine.CoverageSlice) ([]byte, error) {
	b, err := json.MarshalIndent(CoverageReport{Coverage: leg, Slices: sortedSlices(slices)}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("report: coverage json: %w", err)
	}

	return b, nil
}

// RenderCoverageText renders the incident-channel coverage block: the headline
// ratio, then the per-slice attribution so a sub-100% number names exactly
// where telemetry and the ledger diverge. An ungrounded leg prints its reason
// and no ratio — a 0% here would be a claim (ADR-0017).
func RenderCoverageText(leg engine.CoverageLeg, slices []engine.CoverageSlice) string {
	var b strings.Builder
	if leg.Unavailable != "" {
		fmt.Fprintf(&b, "COVERAGE   unavailable: %s\n", leg.Unavailable)
		return b.String()
	}

	fmt.Fprintf(&b, "COVERAGE   [%s] %.1f%% reconciled against %s\n", leg.Evidence, leg.Ratio*100, leg.Source)
	for _, s := range sortedSlices(slices) {
		fmt.Fprintf(&b, "  %-16s %s  telemetry %s  ledger %s  (%.1f%%)\n",
			s.Flow, s.Currency,
			biz.Money{Amount: s.TelemetryMinor, Currency: s.Currency, Exponent: s.Exponent}.String(),
			biz.Money{Amount: s.LedgerMinor, Currency: s.Currency, Exponent: s.Exponent}.String(),
			s.Ratio*100)
	}

	return b.String()
}

// RenderCoverageMarkdown renders the trust line for a postmortem, the format
// the impact report already offers — coverage is what makes the other four
// legs defensible, so it belongs in the same document.
func RenderCoverageMarkdown(leg engine.CoverageLeg, slices []engine.CoverageSlice) string {
	var b strings.Builder
	// "reconciled" distinguishes this from the impact report's own "## Coverage"
	// section, which says only that the number needs a ledger — the two are
	// meant to be pasted into one postmortem.
	b.WriteString("## Coverage (reconciled)\n\n")
	if leg.Unavailable != "" {
		fmt.Fprintf(&b, "_Unavailable: %s_\n", leg.Unavailable)
		return b.String()
	}

	fmt.Fprintf(&b, "**%.1f%%** reconciled against `%s` · evidence `%s`\n\n",
		leg.Ratio*100, leg.Source, leg.Evidence)
	b.WriteString("_The headline is the worst (flow, currency) slice, not the average — a trust number is a weakest-link number (ADR-0011)._\n\n")

	b.WriteString("| Flow | Currency | Telemetry | Ledger | Coverage |\n|---|---|---|---|---|\n")
	for _, s := range sortedSlices(slices) {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %.1f%% |\n",
			s.Flow, s.Currency,
			biz.Money{Amount: s.TelemetryMinor, Currency: s.Currency, Exponent: s.Exponent}.String(),
			biz.Money{Amount: s.LedgerMinor, Currency: s.Currency, Exponent: s.Exponent}.String(),
			s.Ratio*100)
	}

	return b.String()
}
