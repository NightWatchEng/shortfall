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
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/engine/report"
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
		format     = fs.String("format", "text", "output format: "+formatUsage())
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
		wln(stderr, "usage: shortfall reconcile --registry r.yaml --from <RFC3339> --to <RFC3339> --ledger rows.json [--flow f]... [--prometheus URL] [--sql DSN] [--source label] [--format "+formatUsage()+"]")
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

	if !knownFormat(*format) {
		wf(stderr, "--format: unknown %q (want %s)\n", *format, formatUsage())
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

	return renderFormat(stdout, stderr, *format, renderings{
		text:     func() string { return report.RenderCoverageText(leg, slices) },
		markdown: func() string { return report.RenderCoverageMarkdown(leg, slices) },
		json:     func() ([]byte, error) { return report.RenderCoverageJSON(leg, slices) },
	})
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
