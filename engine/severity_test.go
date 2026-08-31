// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

func severityRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Parse([]byte(`version: 1
segments: [smb]
severity:
  - { sev: SEV1, min_per_minute: 100000 }
  - { sev: SEV2, min_per_minute: 10000 }
  - { sev: SEV3, min_per_minute: 1000 }
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: capture, signals: ["queue:cap.q"] }
    sla:
      capture: { deadline: PT30M, on_breach: lost }
    estimator: { default_minor: 100 }
    baseline: { seasonality: hour_of_week, lookback_weeks: 8 }
    recovery: { model: usage_loss_curve, recovered_fraction: 0.5, within: PT2H }
    reconcile: { source: "sql:x" }
`))
	if err != nil {
		t.Fatal(err)
	}

	return &reg
}

// reportWith builds a minimal report over a 10-minute window with the given
// realized/deferred per-currency values.
func reportWith(realized, deferred map[string]int64) Report {
	start := time.Unix(0, 0).UTC()
	return Report{
		Request:  Request{Window: query.TimeRange{From: start, To: start.Add(10 * time.Minute)}},
		Realized: Leg{ByCurrency: realized},
		Deferred: DeferredLeg{Leg: Leg{ByCurrency: deferred}},
	}
}

func TestSuggestSeverity(t *testing.T) {
	reg := severityRegistry(t)
	cases := []struct {
		name     string
		realized map[string]int64
		deferred map[string]int64
		want     string
	}{
		// 10-minute window; rate = at-risk / 10.
		{"exactly the SEV1 floor triggers (>=)", map[string]int64{"USD": 1_000_000}, nil, "SEV1"}, // 1_000_000/10 = 100000
		{"well above the top floor is still SEV1", map[string]int64{"USD": 50_000_000}, nil, "SEV1"},
		{"just below SEV1 -> SEV2", map[string]int64{"USD": 999_990}, nil, "SEV2"},                                  // 99999/min
		{"only clears SEV3", map[string]int64{"USD": 20_000}, nil, "SEV3"},                                          // 2000/min
		{"nothing clears the lowest floor", map[string]int64{"USD": 5_000}, nil, ""},                                // 500/min < 1000
		{"realized + deferred combine", map[string]int64{"USD": 600_000}, map[string]int64{"USD": 600_000}, "SEV1"}, // 1.2M/10=120000
		{"per-currency max wins (EUR SEV3, USD SEV1)", map[string]int64{"USD": 1_000_000, "EUR": 20_000}, nil, "SEV1"},
		{"deferred-only currency (30000/min -> SEV2)", nil, map[string]int64{"USD": 300_000}, "SEV2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SuggestSeverity(reg, reportWith(c.realized, c.deferred))
			if got != c.want {
				t.Fatalf("SuggestSeverity = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSuggestSeverityNoLadderOrWindow(t *testing.T) {
	reg := severityRegistry(t)
	// No ladder -> no suggestion.
	noLadder, err := registry.Parse([]byte(`version: 1
segments: [smb]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages: [{ name: capture, signals: ["q:c"] }]
    sla: { capture: { deadline: PT30M, on_breach: lost } }
    estimator: { default_minor: 100 }
    baseline: { seasonality: hour_of_week, lookback_weeks: 8 }
    recovery: { model: usage_loss_curve, recovered_fraction: 0.5, within: PT2H }
    reconcile: { source: "sql:x" }
`))
	if err != nil {
		t.Fatal(err)
	}

	if s := SuggestSeverity(&noLadder, reportWith(map[string]int64{"USD": 9_999_999}, nil)); s != "" {
		t.Fatalf("no ladder must give no suggestion, got %q", s)
	}

	if s := SuggestSeverity(nil, reportWith(map[string]int64{"USD": 9_999_999}, nil)); s != "" {
		t.Fatalf("nil registry must give no suggestion, got %q", s)
	}

	// Zero-length window -> no rate -> no suggestion.
	zero := Report{
		Request:  Request{Window: query.TimeRange{From: time.Unix(0, 0), To: time.Unix(0, 0)}},
		Realized: Leg{ByCurrency: map[string]int64{"USD": 9_999_999}},
	}
	if s := SuggestSeverity(reg, zero); s != "" {
		t.Fatalf("zero window must give no suggestion, got %q", s)
	}
}
