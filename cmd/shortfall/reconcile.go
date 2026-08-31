// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// runReconcile implements `shortfall reconcile`: compare telemetry against a
// provider-side ledger and publish the coverage ratio (ADR-0011). The ledger is
// a JSON array of biz.LedgerRow — the output of the Stripe reconciler
// (stripe.Reconcile) or a SQL ledger job — so this command needs no live
// provider credentials of its own.
func runReconcile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		regPath    = fs.String("registry", "", "path to the flow registry YAML (required)")
		fromStr    = fs.String("from", "", "window start, RFC3339 (required)")
		toStr      = fs.String("to", "", "window end, RFC3339 (required)")
		ledgerPath = fs.String("ledger", "", "path to a JSON array of ledger rows (required)")
		source     = fs.String("source", "", "ledger source label for the report (defaults to the ledger path)")
		promURL    = fs.String("prometheus", "", "Prometheus base URL for telemetry")
		sqlDSN     = fs.String("sql", "", "SQL DSN for telemetry")
		sqlDriver  = fs.String("sql-driver", "sqlite", "database/sql driver name for --sql")
		flows      stringList
	)
	fs.Var(&flows, "flow", "flow to include (repeatable); omit for all in the ledger")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *regPath == "" || *fromStr == "" || *toStr == "" || *ledgerPath == "" {
		wln(stderr, "usage: shortfall reconcile --registry r.yaml --from <RFC3339> --to <RFC3339> --ledger rows.json [--flow f]... [--prometheus URL] [--sql DSN] [--source label]")
		return 2
	}

	reg, err := registry.Load(*regPath)
	if err != nil {
		wf(stderr, "registry: %v\n", err)
		return 1
	}

	from, err := time.Parse(time.RFC3339, *fromStr)
	if err != nil {
		wf(stderr, "--from: %v\n", err)
		return 2
	}

	to, err := time.Parse(time.RFC3339, *toStr)
	if err != nil {
		wf(stderr, "--to: %v\n", err)
		return 2
	}

	ledger, err := loadLedger(*ledgerPath)
	if err != nil {
		wf(stderr, "--ledger: %v\n", err)
		return 1
	}

	src := *source
	if src == "" {
		src = *ledgerPath
	}

	q, cleanup, err := buildQuerier(*promURL, *sqlDSN, *sqlDriver)
	if err != nil {
		wf(stderr, "%v\n", err)
		return 2
	}

	defer cleanup()

	leg, slices, err := engine.Coverage(context.Background(), &reg, q,
		engine.Request{Window: query.TimeRange{From: from, To: to}, Flows: flows}, ledger, src)
	if err != nil {
		wf(stderr, "coverage: %v\n", err)
		return 1
	}

	renderCoverage(stdout, leg, slices)
	return 0
}

// loadLedger reads a JSON array of ledger rows and validates each — a malformed
// ledger must fail loudly, not silently skew the coverage number.
func loadLedger(path string) ([]biz.LedgerRow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var rows []biz.LedgerRow
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, err
	}

	for i, r := range rows {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
	}

	return rows, nil
}

// renderCoverage prints the coverage headline and the per-slice attribution so a
// sub-100% number names exactly where telemetry and the ledger diverge.
func renderCoverage(w io.Writer, leg engine.CoverageLeg, slices []engine.CoverageSlice) {
	if leg.Unavailable != "" {
		wf(w, "COVERAGE   unavailable: %s\n", leg.Unavailable)
		return
	}

	wf(w, "COVERAGE   [%s] %.1f%% reconciled against %s\n", leg.Evidence, leg.Ratio*100, leg.Source)
	sort.Slice(slices, func(i, j int) bool {
		if slices[i].Flow != slices[j].Flow {
			return slices[i].Flow < slices[j].Flow
		}

		return slices[i].Currency < slices[j].Currency
	})
	for _, s := range slices {
		wf(w, "  %-16s %s  telemetry %s  ledger %s  (%.1f%%)\n",
			s.Flow, s.Currency,
			biz.Money{Amount: s.TelemetryMinor, Currency: s.Currency, Exponent: s.Exponent}.String(),
			biz.Money{Amount: s.LedgerMinor, Currency: s.Currency, Exponent: s.Exponent}.String(),
			s.Ratio*100)
	}
}
