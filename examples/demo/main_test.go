// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/NightWatchEng/shortfall/engine"
)

// TestRunRendersAReportWithItsEvidenceLabels pins what the demo is FOR. It is
// the first thing a stranger runs, so the failure that matters is not a crash
// — it is a report that renders while saying nothing, or one whose evidence
// labels have gone missing and left bare numbers a reader would take as
// equally solid. The amounts are deliberately not asserted: they move with
// the seed and the platform, and pinning them here would make this a golden
// test that fails for the wrong reason.
func TestRunRendersAReportWithItsEvidenceLabels(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(&stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	cases := []struct {
		name string
		want string
		why  string
	}{
		{name: "flow header", want: "BUSINESS IMPACT — invoice.pay", why: "the report did not render"},
		{name: "realized leg", want: "REALIZED   [deterministic]", why: "the deterministic leg lost its evidence label"},
		{name: "unrealized leg", want: "UNREALIZED [estimate]", why: "the estimated leg lost its evidence label"},
		{name: "customers leg", want: "CUSTOMERS  ", why: "customer impact did not render"},
		{
			name: "estimate carries its do-not-add warning",
			want: "do not add to realized",
			why:  "keeping realized and estimated separate is what the demo exists to show",
		},
		{
			// Anchored to the leg. A bare "unavailable:" also matches CUSTOMERS
			// and a degraded UNREALIZED note, so it would pass while the demo
			// was failing in a different place entirely.
			name: "coverage names its reason instead of reporting zero",
			want: "COVERAGE   unavailable:",
			why: "engine.Compute never computes coverage — it is a reconcile-time number needing " +
				"a provider ledger — and ADR-0017 says such a leg names its reason rather than " +
				"rendering a measured zero. A demo printing 0% would teach the opposite",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(out, c.want) {
				t.Fatalf("output does not contain %q — %s.\n---\n%s", c.want, c.why, out)
			}
		})
	}
}

// TestUnrealizedIsARangeNotAPoint guards the demo's most fragile assumption.
// The registry fits an hour-of-week baseline over lookbackWeeks, and run must
// simulate at least that much history before the incident. If it ever
// simulates less, the leg silently collapses to a point presented as a range
// — every digit in the README's published block changes, the paragraph
// teaching "always a range, never a point" becomes false, and nothing else in
// this suite notices.
func TestUnrealizedIsARangeNotAPoint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(&stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()

	// A belt to the point check's braces, and honest about being one: the
	// in-memory querier declares 520 weeks of history unconditionally, so
	// engine.Unrealized's retention note cannot fire here today. It would if
	// that ever became a real capability, and it costs nothing to keep.
	if strings.Contains(out, "RETENTION GAP") || strings.Contains(out, "no baseline history") {
		t.Fatalf("the estimate is degraded — the querier now reports real history "+
			"and it is short of the registry's lookback.\n---\n%s", out)
	}

	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "UNREALIZED") {
			line = l
			break
		}
	}

	if line == "" {
		t.Fatalf("no UNREALIZED line in the report:\n%s", out)
	}

	lo, rest, ok := strings.Cut(strings.TrimPrefix(line, "UNREALIZED [estimate] "), " … ")
	if !ok || lo == "" || rest == "" {
		t.Fatalf("UNREALIZED line is not a low … high range: %q", line)
	}

	// `rest` is the high bound followed by "(mid …)" and the counterfactual
	// note, so the degenerate case is the high bound repeating the low one.
	if strings.HasPrefix(rest, lo+" ") || rest == lo {
		t.Fatalf("the unrealized leg is a point, not a range (%q). Counterfactual demand can "+
			"only be sized as an interval; a point here means the baseline had nothing to fit "+
			"against", line)
	}
}

// TestRenderRefusesAnEmptyReport enters the guard directly. The earlier
// version of this test called run and asserted a string was ABSENT from
// stdout, which passed whenever run failed for any reason at all — including
// reasons that never reach the guard. It could not distinguish "the guard
// held" from "nothing ran".
func TestRenderRefusesAnEmptyReport(t *testing.T) {
	cases := []struct {
		name     string
		rep      engine.Report
		wantCode int
		wantErr  string
	}{
		{
			name:     "an empty realized leg is refused",
			rep:      engine.Report{Realized: engine.Leg{Count: 0}},
			wantCode: 1,
			wantErr:  "the realized leg is empty",
		},
		{
			name:     "a populated realized leg renders",
			rep:      engine.Report{Realized: engine.Leg{Count: 1, ByCurrency: map[string]int64{"USD": 100}}},
			wantCode: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := render(&stdout, &stderr, c.rep)
			if code != c.wantCode {
				t.Fatalf("render = %d, want %d (stderr: %q)", code, c.wantCode, stderr.String())
			}

			if c.wantErr == "" {
				if stdout.Len() == 0 {
					t.Fatal("nothing was written — a report that renders nothing is the failure this guards")
				}

				return
			}

			if stdout.Len() != 0 {
				t.Fatalf("refused the report but still wrote to stdout: %q", stdout.String())
			}

			if !strings.Contains(stderr.String(), c.wantErr) {
				t.Fatalf("stderr = %q, want it to explain the refusal (%q)", stderr.String(), c.wantErr)
			}
		})
	}
}
