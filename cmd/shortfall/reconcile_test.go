// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine/report"
)

func writeLedger(t *testing.T, json string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func runReconcileCLI(t *testing.T, dsn, ledgerPath string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runReconcile([]string{
		"--registry", filepath.Join("testdata", "registry.yaml"),
		"--from", "2026-08-27T14:00:00Z",
		"--to", "2026-08-27T15:00:00Z",
		"--flow", "invoice.pay",
		"--sql", dsn, "--sql-driver", "sqlite",
		"--ledger", ledgerPath,
		"--source", "sql:ledger.payments",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errb.String())
	}

	return out.String(), code
}

func TestReconcileEndToEnd100(t *testing.T) {
	base := time.Date(2026, 8, 27, 14, 30, 0, 0, time.UTC)
	at := func(m int) int64 { return base.Add(time.Duration(m) * time.Minute).UnixNano() }
	// Telemetry: two settled successes, 5000 + 5000 = 10000 USD.
	dsn := "file:" + seedSQLite(t, [][]any{
		{"invoice.pay", "settle", "success", "USD", "smb", "fee", "h:c1", "inv_1", 5000, at(1)},
		{"invoice.pay", "settle", "success", "USD", "smb", "fee", "h:c2", "inv_2", 5000, at(2)},
	})
	// Ledger records the same 10000 USD → 100% coverage.
	ledger := writeLedger(t, `[{"Flow":"invoice.pay","Outcome":"success","Money":{"Amount":10000,"Currency":"USD","Exponent":2},"Count":2}]`)

	out, _ := runReconcileCLI(t, dsn, ledger)
	if !strings.Contains(out, "100.0% reconciled against sql:ledger.payments") {
		t.Fatalf("expected 100%% coverage, got:\n%s", out)
	}
}

func TestReconcileEndToEndDroppedExporter(t *testing.T) {
	base := time.Date(2026, 8, 27, 14, 30, 0, 0, time.UTC)
	at := func(m int) int64 { return base.Add(time.Duration(m) * time.Minute).UnixNano() }
	// Telemetry saw only 10000 USD...
	dsn := "file:" + seedSQLite(t, [][]any{
		{"invoice.pay", "settle", "success", "USD", "smb", "fee", "h:c1", "inv_1", 10000, at(1)},
	})
	// ...but the ledger recorded 20000 USD — an exporter dropped half.
	ledger := writeLedger(t, `[{"Flow":"invoice.pay","Outcome":"success","Money":{"Amount":20000,"Currency":"USD","Exponent":2},"Count":2}]`)

	out, _ := runReconcileCLI(t, dsn, ledger)
	if !strings.Contains(out, "50.0% reconciled") {
		t.Fatalf("expected 50%% coverage, got:\n%s", out)
	}

	// The delta is attributed on the slice line: telemetry USD 100.00, ledger USD 200.00.
	if !strings.Contains(out, "telemetry USD 100.00") || !strings.Contains(out, "ledger USD 200.00") {
		t.Fatalf("expected per-slice attribution, got:\n%s", out)
	}
}

func TestReconcileMalformedLedgerFails(t *testing.T) {
	dsn := "file:" + seedSQLite(t, nil)
	bad := writeLedger(t, `[{"Flow":"invoice.pay","Outcome":"bogus","Money":{"Amount":1,"Currency":"USD","Exponent":2},"Count":1}]`)
	var out, errb bytes.Buffer
	code := runReconcile([]string{
		"--registry", filepath.Join("testdata", "registry.yaml"),
		"--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z",
		"--sql", dsn, "--sql-driver", "sqlite", "--ledger", bad,
	}, &out, &errb)
	if code == 0 {
		t.Fatal("a malformed ledger row must fail, not silently skew coverage")
	}
}

// runReconcileFormat runs reconcile with an explicit --format, returning
// stdout, stderr and the exit code without failing the test — the unknown
// -format cases need the non-zero exit.
func runReconcileFormat(t *testing.T, dsn, ledgerPath, format string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runReconcile([]string{
		"--registry", filepath.Join("testdata", "registry.yaml"),
		"--from", "2026-08-27T14:00:00Z",
		"--to", "2026-08-27T15:00:00Z",
		"--flow", "invoice.pay",
		"--sql", dsn, "--sql-driver", "sqlite",
		"--ledger", ledgerPath,
		"--source", "sql:ledger.payments",
		"--format", format,
	}, &out, &errb)

	return out.String(), errb.String(), code
}

// halfCoveredLedger seeds telemetry that saw USD 100.00 against a ledger
// recording USD 200.00 — a 50% slice, so every format has a real number and a
// real attribution to render.
func halfCoveredLedger(t *testing.T) (dsn, ledger string) {
	t.Helper()
	base := time.Date(2026, 8, 27, 14, 30, 0, 0, time.UTC)
	at := func(m int) int64 { return base.Add(time.Duration(m) * time.Minute).UnixNano() }
	dsn = "file:" + seedSQLite(t, [][]any{
		{"invoice.pay", "settle", "success", "USD", "smb", "fee", "h:c1", "inv_1", 10000, at(1)},
	})
	ledger = writeLedger(t, `[{"Flow":"invoice.pay","Outcome":"success","Money":{"Amount":20000,"Currency":"USD","Exponent":2},"Count":2}]`)

	return dsn, ledger
}

func TestReconcileRendersEveryFormat(t *testing.T) {
	dsn, ledger := halfCoveredLedger(t)

	cases := []struct {
		name   string
		format string
		want   string
	}{
		{"text is the default rendering", "text", "COVERAGE   [trust] 50.0% reconciled against sql:ledger.payments"},
		{"markdown carries a postmortem table", "markdown", "| Flow | Currency | Telemetry | Ledger | Coverage |"},
		{"markdown states the worst-slice rule", "markdown", "weakest-link"},
		{"json carries the coverage object", "json", `"coverage"`},
		{"json carries the slice attribution", "json", `"slices"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errb, code := runReconcileFormat(t, dsn, ledger, c.format)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errb)
			}

			if !strings.Contains(out, c.want) {
				t.Errorf("--format %s missing %q, got:\n%s", c.format, c.want, out)
			}
		})
	}
}

func TestReconcileJSONRoundTrips(t *testing.T) {
	dsn, ledger := halfCoveredLedger(t)
	out, errb, code := runReconcileFormat(t, dsn, ledger, "json")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errb)
	}

	var got report.CoverageReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--format json did not emit parseable JSON: %v\n%s", err, out)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"ratio is the worst slice", got.Coverage.Ratio, 0.5},
		{"source names the ledger", got.Coverage.Source, "sql:ledger.payments"},
		{"the slice is attributed", len(got.Slices), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
}

// TestFormatVocabularyIsShared is the anti-drift pin: every
// name in formatVocabulary must render for BOTH verbs, and an unknown name must
// be rejected the same way by each. Adding a name to the vocabulary without
// wiring it into renderFormat fails here rather than at a user's terminal.
func TestFormatVocabularyIsShared(t *testing.T) {
	dsn, ledger := halfCoveredLedger(t)
	reg := filepath.Join("testdata", "registry.yaml")

	for _, format := range formatVocabulary {
		t.Run("reconcile renders "+format, func(t *testing.T) {
			out, errb, code := runReconcileFormat(t, dsn, ledger, format)
			if code != 0 {
				t.Fatalf("reconcile --format %s = %d, stderr: %s", format, code, errb)
			}

			if strings.TrimSpace(out) == "" {
				t.Errorf("reconcile --format %s rendered nothing", format)
			}
		})

		t.Run("impact renders "+format, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := runImpact([]string{
				"--registry", reg,
				"--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z",
				"--sql", dsn, "--sql-driver", "sqlite", "--format", format,
			}, &out, &errb)
			if code != 0 {
				t.Fatalf("impact --format %s = %d, stderr: %s", format, code, errb.String())
			}

			if strings.TrimSpace(out.String()) == "" {
				t.Errorf("impact --format %s rendered nothing", format)
			}
		})
	}

	// Both verbs reject an unknown name, with the same vocabulary in the
	// message — and reject it BEFORE querying a backend.
	cases := []struct {
		name string
		run  func() (string, int)
	}{
		{"reconcile", func() (string, int) {
			_, errb, code := runReconcileFormat(t, dsn, ledger, "yaml")

			return errb, code
		}},
		{"impact", func() (string, int) {
			var out, errb bytes.Buffer
			code := runImpact([]string{
				"--registry", reg,
				"--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z",
				"--sql", dsn, "--sql-driver", "sqlite", "--format", "yaml",
			}, &out, &errb)

			return errb.String(), code
		}},
	}
	for _, c := range cases {
		t.Run(c.name+" rejects an unknown format", func(t *testing.T) {
			errb, code := c.run()
			if code != 2 {
				t.Fatalf("%s --format yaml = %d, want 2", c.name, code)
			}

			if !strings.Contains(errb, "want "+formatUsage()) {
				t.Errorf("%s must name the format vocabulary on rejection, got: %s", c.name, errb)
			}
		})
	}
}
