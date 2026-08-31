// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	stdsql "database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// stubPrometheus serves a canned Prometheus HTTP API response.
func stubPrometheus(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// seedSQLite writes an outcomes table to a temp SQLite file and returns a DSN.
func seedSQLite(t *testing.T, rows [][]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	db, err := stdsql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE biz_outcomes (
		flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
		kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER, at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO biz_outcomes VALUES (?,?,?,?,?,?,?,?,?,?)`, r...); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestImpactEndToEndSQLite runs `shortfall impact` against a SQLite-backed
// event store and checks the rendered ledger block — the whole CLI path
// (flags → registry → querier → Compute → render) in ordinary CI.
func TestImpactEndToEndSQLite(t *testing.T) {
	base := time.Date(2026, 8, 27, 14, 30, 0, 0, time.UTC)
	at := func(m int) int64 { return base.Add(time.Duration(m) * time.Minute).UnixNano() }
	rows := [][]any{
		{"invoice.pay", "capture", "failed", "USD", "smb", "fee", "h:c1", "inv_1", 14900, at(1)},
		{"invoice.pay", "capture", "failed", "USD", "smb", "fee", "h:c2", "inv_2", 100, at(2)},
		{"invoice.pay", "auth", "success", "USD", "smb", "fee", "h:c3", "inv_3", 5000, at(3)},
	}
	dsn := "file:" + seedSQLite(t, rows)

	var out, errb bytes.Buffer
	code := runImpact([]string{
		"--registry", filepath.Join("testdata", "registry.yaml"),
		"--from", "2026-08-27T14:00:00Z",
		"--to", "2026-08-27T15:00:00Z",
		"--flow", "invoice.pay",
		"--sql", dsn, "--sql-driver", "sqlite",
		"--format", "text",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errb.String())
	}
	got := out.String()

	// Realized: two failed USD txns, 15000 minor units.
	if !strings.Contains(got, "REALIZED") || !strings.Contains(got, "USD 15000") {
		t.Fatalf("missing realized USD 15000:\n%s", got)
	}
	// Customers computed from events (2 distinct failed).
	if !strings.Contains(got, "CUSTOMERS") || !strings.Contains(got, "2 distinct") {
		t.Fatalf("missing customers:\n%s", got)
	}
	// Deferred needs metrics; an events-only backend marks it unavailable.
	if !strings.Contains(got, "DEFERRED") {
		t.Fatalf("missing deferred line:\n%s", got)
	}
	// Evidence tags present.
	if !strings.Contains(got, "deterministic") {
		t.Fatalf("missing evidence tag:\n%s", got)
	}
}

func TestImpactJSONFormat(t *testing.T) {
	dsn := "file:" + seedSQLite(t, [][]any{
		{"invoice.pay", "capture", "failed", "USD", "smb", "fee", "h:c1", "inv_1", 14900, time.Now().UnixNano()},
	})
	var out, errb bytes.Buffer
	code := runImpact([]string{
		"--registry", filepath.Join("testdata", "registry.yaml"),
		"--from", "2020-01-01T00:00:00Z", "--to", "2030-01-01T00:00:00Z",
		"--sql", dsn, "--format", "json",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Fatalf("json output expected, got:\n%s", out.String())
	}
}

func TestImpactRequiresFlagsAndQuerier(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing registry", []string{"--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z", "--sql", "x"}},
		{"missing window", []string{"--registry", filepath.Join("testdata", "registry.yaml"), "--sql", "x"}},
		{"no querier", []string{"--registry", filepath.Join("testdata", "registry.yaml"), "--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z"}},
		{"bad scope", []string{"--registry", filepath.Join("testdata", "registry.yaml"), "--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z", "--prometheus", "http://x", "--scope", "bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runImpact(c.args, &out, &errb); code == 0 {
				t.Fatalf("expected non-zero exit; stdout:\n%s", out.String())
			}
		})
	}
}

// TestImpactPrometheusPath points --prometheus at a stub that returns a canned
// vector, proving the metric-leg wiring end to end (no real Prometheus).
func TestImpactPrometheusPath(t *testing.T) {
	// A tiny handler standing in for the Prometheus HTTP API.
	stub := stubPrometheus(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	defer stub.Close()
	var out, errb bytes.Buffer
	code := runImpact([]string{
		"--registry", filepath.Join("testdata", "registry.yaml"),
		"--from", "2026-08-27T14:00:00Z", "--to", "2026-08-27T15:00:00Z",
		"--flow", "invoice.pay", "--prometheus", stub.URL, "--format", "text",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// Metrics-only backend: customers leg is honestly unavailable.
	if !strings.Contains(out.String(), "CUSTOMERS  unavailable") {
		t.Fatalf("metrics-only run must mark customers unavailable:\n%s", out.String())
	}
}
