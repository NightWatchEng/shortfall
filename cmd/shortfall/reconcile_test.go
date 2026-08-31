// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
