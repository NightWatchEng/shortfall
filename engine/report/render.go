package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/NightWatchEng/shortfall/engine"
)

// RenderJSON renders the report as indented JSON. The evidence tags and the
// separate legs ride the struct, so no summing can happen here.
func RenderJSON(r engine.Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("report: json: %w", err)
	}
	return b, nil
}

// money renders a per-currency minor-unit map deterministically, e.g.
// "USD 15000, EUR 700" — never summed across currencies (ADR-0001), never a
// float. Empty renders as "none".
func money(byCur map[string]int64) string {
	if len(byCur) == 0 {
		return "none"
	}
	curs := make([]string, 0, len(byCur))
	for c := range byCur {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	parts := make([]string, 0, len(curs))
	for _, c := range curs {
		parts = append(parts, fmt.Sprintf("%s %d", c, byCur[c]))
	}
	return strings.Join(parts, ", ")
}

// RenderText renders the incident-channel ledger block. Realized and
// unrealized are separate lines; there is deliberately no combined total.
func RenderText(r engine.Report) string {
	var b strings.Builder
	flows := strings.Join(r.Request.Flows, ", ")
	if flows == "" {
		flows = "(all flows)"
	}
	fmt.Fprintf(&b, "BUSINESS IMPACT — %s\n", flows)
	fmt.Fprintf(&b, "window %s → %s · registry v%d · %s\n",
		r.Request.Window.From.UTC().Format("2006-01-02T15:04Z"),
		r.Request.Window.To.UTC().Format("2006-01-02T15:04Z"),
		r.RegistryVersion, r.LibraryVersion)
	b.WriteString("amounts are minor currency units; realized and estimate are never added\n\n")

	fmt.Fprintf(&b, "REALIZED   [%s] %s\n", r.Realized.Evidence, money(r.Realized.ByCurrency))
	for _, c := range r.Realized.Caveats {
		fmt.Fprintf(&b, "           caveat: %s\n", c)
	}

	fmt.Fprintf(&b, "DEFERRED   [%s] %s in-flight\n", r.Deferred.Evidence, money(r.Deferred.ByCurrency))
	if len(r.Deferred.ProjectedLostMinor) > 0 {
		fmt.Fprintf(&b, "           projected lost if SLAs breach: %s\n", money(r.Deferred.ProjectedLostMinor))
	}
	if r.Deferred.OldestAgeMinutes > 0 {
		fmt.Fprintf(&b, "           oldest in-flight ≥ %d min\n", r.Deferred.OldestAgeMinutes)
	}

	// Unrealized is an estimate range, tagged, and never added to realized.
	fmt.Fprintf(&b, "UNREALIZED [%s] %s … %s (mid %s)  — counterfactual; do not add to realized\n",
		r.Unrealized.Evidence, money(r.Unrealized.LowMinor), money(r.Unrealized.HighMinor), money(r.Unrealized.MidMinor))
	for _, n := range r.Unrealized.Notes {
		fmt.Fprintf(&b, "           note: %s\n", n)
	}

	if r.Customers.NotAvailableReason != "" {
		fmt.Fprintf(&b, "CUSTOMERS  unavailable: %s\n", r.Customers.NotAvailableReason)
	} else {
		fmt.Fprintf(&b, "CUSTOMERS  %d distinct (%s)\n", r.Customers.Distinct, segmentList(r.Customers.BySegment))
		for _, c := range r.Customers.TopN {
			fmt.Fprintf(&b, "           %s [%s] %s\n", c.CustomerID, c.Segment, money(c.ByCurrency))
		}
	}

	if r.Coverage.Unavailable != "" {
		fmt.Fprintf(&b, "COVERAGE   unavailable: %s\n", r.Coverage.Unavailable)
	} else {
		fmt.Fprintf(
			&b,
			"COVERAGE   [%s] %.1f%% reconciled (%s)\n",
			r.Coverage.Evidence,
			r.Coverage.Ratio*100,
			r.Coverage.Source,
		)
	}

	if r.Severity != "" {
		fmt.Fprintf(&b, "\nSeverity: %s (suggested)\n", r.Severity)
	}
	return b.String()
}

// segmentList renders BySegment deterministically, e.g. "smb 30, enterprise 12".
func segmentList(bySeg map[string]int64) string {
	if len(bySeg) == 0 {
		return "no segments"
	}
	segs := make([]string, 0, len(bySeg))
	for s := range bySeg {
		segs = append(segs, s)
	}
	sort.Strings(segs)
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		label := s
		if label == "" {
			label = "(unsegmented)"
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, bySeg[s]))
	}
	return strings.Join(parts, ", ")
}

// RenderMarkdown renders the report for a postmortem.
func RenderMarkdown(r engine.Report) string {
	var b strings.Builder
	flows := strings.Join(r.Request.Flows, ", ")
	if flows == "" {
		flows = "(all flows)"
	}
	fmt.Fprintf(&b, "# Business impact — %s\n\n", flows)
	fmt.Fprintf(&b, "Window `%s → %s` · registry v%d · %s\n\n",
		r.Request.Window.From.UTC().Format("2006-01-02T15:04Z"),
		r.Request.Window.To.UTC().Format("2006-01-02T15:04Z"),
		r.RegistryVersion, r.LibraryVersion)
	b.WriteString("_Amounts are minor currency units (ADR-0001). Realized value and estimated value are reported separately and never summed._\n\n")

	b.WriteString("| Leg | Evidence | Value |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Realized | %s | %s |\n", r.Realized.Evidence, money(r.Realized.ByCurrency))
	fmt.Fprintf(&b, "| Deferred (in-flight) | %s | %s |\n", r.Deferred.Evidence, money(r.Deferred.ByCurrency))
	fmt.Fprintf(
		&b,
		"| Deferred → projected lost | %s | %s |\n",
		r.Deferred.Evidence,
		money(r.Deferred.ProjectedLostMinor),
	)
	fmt.Fprintf(&b, "| Unrealized (counterfactual) | %s | %s … %s (mid %s) |\n",
		r.Unrealized.Evidence, money(r.Unrealized.LowMinor), money(r.Unrealized.HighMinor), money(r.Unrealized.MidMinor))
	b.WriteString("\n> Unrealized is an estimate range and must not be added to realized.\n\n")

	b.WriteString("## Customers\n\n")
	if r.Customers.NotAvailableReason != "" {
		fmt.Fprintf(&b, "_Unavailable: %s_\n\n", r.Customers.NotAvailableReason)
	} else {
		fmt.Fprintf(&b, "%d distinct affected (%s).\n\n", r.Customers.Distinct, segmentList(r.Customers.BySegment))
		if len(r.Customers.TopN) > 0 {
			b.WriteString("| Customer | Segment | Failed value |\n|---|---|---|\n")
			for _, c := range r.Customers.TopN {
				fmt.Fprintf(&b, "| %s | %s | %s |\n", c.CustomerID, c.Segment, money(c.ByCurrency))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("## Coverage\n\n")
	if r.Coverage.Unavailable != "" {
		fmt.Fprintf(&b, "_Unavailable: %s_\n", r.Coverage.Unavailable)
	} else {
		fmt.Fprintf(
			&b,
			"%.1f%% of telemetry reconciled against `%s` [%s].\n",
			r.Coverage.Ratio*100,
			r.Coverage.Source,
			r.Coverage.Evidence,
		)
	}
	if r.Severity != "" {
		fmt.Fprintf(&b, "\n**Suggested severity:** %s\n", r.Severity)
	}
	return b.String()
}
