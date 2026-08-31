// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"fmt"
	"strings"

	"github.com/NightWatchEng/shortfall/engine"
)

// Summary renders the one-paragraph impact line the incident-tool writers
// PATCH into vendor impact/custom fields: every leg keeps its own evidence
// tag, the counterfactual stays a range, and realized is never summed with
// estimate. Amounts are minor currency units (the line says so).
func Summary(r engine.Report) string {
	var b strings.Builder
	flows := strings.Join(r.Request.Flows, ", ")
	if flows == "" {
		flows = "(all flows)"
	}

	fmt.Fprintf(&b, "shortfall impact %s %s→%s (minor units): ",
		flows,
		r.Request.Window.From.UTC().Format("2006-01-02T15:04Z"),
		r.Request.Window.To.UTC().Format("2006-01-02T15:04Z"))
	// An ungrounded leg says n/a, never a plausible-looking zero — the
	// vendor field this line lands in is read as a measurement.
	if r.Realized.Unavailable {
		b.WriteString("realized n/a")
	} else {
		fmt.Fprintf(&b, "realized [%s] %s", r.Realized.Evidence, money(r.Realized.ByCurrency))
	}

	if r.Deferred.Unavailable {
		b.WriteString(" · deferred n/a")
	} else {
		fmt.Fprintf(&b, " · deferred [%s] %s in-flight", r.Deferred.Evidence, money(r.Deferred.ByCurrency))
	}

	if r.Unrealized.Unavailable {
		b.WriteString(" · unrealized n/a")
	} else {
		fmt.Fprintf(&b, " · unrealized [%s] %s", r.Unrealized.Evidence, moneyRange(r.Unrealized))
	}

	if r.Customers.NotAvailableReason != "" {
		b.WriteString(" · customers n/a")
	} else {
		fmt.Fprintf(&b, " · customers %d distinct", r.Customers.Distinct)
	}

	if r.Severity != "" {
		fmt.Fprintf(&b, " · suggested %s", r.Severity)
	}

	return b.String()
}

// moneyRange renders an EstLeg as per-currency low–high ranges, currencies
// sorted; "none" when the leg carries no estimate. The engine fills the
// three maps in lockstep, but Summary accepts any Report, so each
// currency's bounds are the min and max over whichever bounds it carries —
// an asymmetric map can never render an inverted range.
func moneyRange(leg engine.EstLeg) string {
	if len(leg.LowMinor) == 0 && len(leg.MidMinor) == 0 && len(leg.HighMinor) == 0 {
		return "none"
	}

	curs := map[string]struct{}{}
	for _, m := range []map[string]int64{leg.LowMinor, leg.MidMinor, leg.HighMinor} {
		for c := range m {
			curs[c] = struct{}{}
		}
	}

	sorted := make([]string, 0, len(curs))
	for c := range curs {
		sorted = append(sorted, c)
	}

	sortStrings(sorted)
	parts := make([]string, 0, len(sorted))
	for _, c := range sorted {
		lo, hi, have := int64(0), int64(0), false
		for _, m := range []map[string]int64{leg.LowMinor, leg.MidMinor, leg.HighMinor} {
			v, ok := m[c]
			if !ok {
				continue
			}

			if !have || v < lo {
				lo = v
			}

			if !have || v > hi {
				hi = v
			}

			have = true
		}

		parts = append(parts, fmt.Sprintf("%s %d–%d", c, lo, hi))
	}

	return strings.Join(parts, ", ")
}
