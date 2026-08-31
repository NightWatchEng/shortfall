// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	stdsql "database/sql"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for --sql

	promql "github.com/NightWatchEng/shortfall/adapters/query/promql"
	sqlq "github.com/NightWatchEng/shortfall/adapters/query/sql"
	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/engine/report"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/registry"
)

// CLI write helpers: a failed write to our own stdout/stderr is unrecoverable,
// so the error is intentionally dropped (and errcheck stays happy without a
// _,_= at every call site).
func wf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func wln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }
func ws(w io.Writer, s string)                { _, _ = io.WriteString(w, s) }

// stringList collects a repeatable flag (e.g. --flow a --flow b).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runImpact implements `shortfall impact`: window + scope + flows in, the
// four-leg report out, rendered from whatever querier the flags configure.
func runImpact(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		regPath   = fs.String("registry", "", "path to the flow registry YAML (required)")
		fromStr   = fs.String("from", "", "window start, RFC3339 (required)")
		toStr     = fs.String("to", "", "window end, RFC3339 (required)")
		format    = fs.String("format", "text", "output format: text|json|markdown")
		promURL   = fs.String("prometheus", "", "Prometheus base URL for the metric legs")
		sqlDSN    = fs.String("sql", "", "SQL DSN for the event legs")
		sqlDriver = fs.String("sql-driver", "sqlite", "database/sql driver name for --sql")
		flows     stringList
		scopes    stringList
	)
	fs.Var(&flows, "flow", "flow to include (repeatable); omit for all")
	fs.Var(&scopes, "scope", "scope filter k=v (repeatable), e.g. --scope stage=capture")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *regPath == "" || *fromStr == "" || *toStr == "" {
		wln(stderr, "usage: shortfall impact --registry r.yaml --from <RFC3339> --to <RFC3339> [--flow f]... [--scope k=v]... [--prometheus URL] [--sql DSN] [--format text|json|markdown]")
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

	scope, err := parseScopes(scopes)
	if err != nil {
		wf(stderr, "%v\n", err)
		return 2
	}

	q, cleanup, err := buildQuerier(*promURL, *sqlDSN, *sqlDriver)
	if err != nil {
		wf(stderr, "%v\n", err)
		return 2
	}

	defer cleanup()

	rep, err := engine.Compute(context.Background(), &reg, q, engine.Request{
		Window: query.TimeRange{From: from, To: to},
		Scope:  scope,
		Flows:  flows,
	})
	if err != nil {
		wf(stderr, "compute: %v\n", err)
		return 1
	}

	switch *format {
	case "text":
		ws(stdout, report.RenderText(rep))
	case "markdown":
		ws(stdout, report.RenderMarkdown(rep))
	case "json":
		b, err := report.RenderJSON(rep)
		if err != nil {
			wf(stderr, "render: %v\n", err)
			return 1
		}

		wln(stdout, string(b))
	default:
		wf(stderr, "--format: unknown %q (want text|json|markdown)\n", *format)
		return 2
	}

	return 0
}

func parseScopes(scopes stringList) (engine.Scope, error) {
	s := engine.Scope{}
	for _, kv := range scopes {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--scope %q: want k=v", kv)
		}

		s[k] = v
	}

	return s, nil
}

// buildQuerier composes a querier from the configured backends: Prometheus for
// metrics, SQL for events. With both, a combining querier routes each verb;
// with one, that backend is used directly. It returns a cleanup for any DB.
func buildQuerier(promURL, sqlDSN, sqlDriver string) (query.Querier, func(), error) {
	noop := func() {}
	var metrics, events query.Querier
	cleanup := noop

	if promURL != "" {
		metrics = promql.New(promURL)
	}

	if sqlDSN != "" {
		db, err := stdsql.Open(sqlDriver, sqlDSN)
		if err != nil {
			return nil, noop, fmt.Errorf("--sql: open: %w", err)
		}

		eq, err := sqlq.New(db)
		if err != nil {
			_ = db.Close()
			return nil, noop, fmt.Errorf("--sql: %w", err)
		}

		events = eq
		cleanup = func() { _ = db.Close() }
	}

	switch {
	case metrics != nil && events != nil:
		return combined{metrics: metrics, events: events}, cleanup, nil
	case metrics != nil:
		return metrics, cleanup, nil
	case events != nil:
		return events, cleanup, nil
	default:
		return nil, noop, fmt.Errorf("no querier configured: pass --prometheus and/or --sql")
	}
}

// combined routes metric queries to one backend and event queries to another —
// the common split of metrics in a TSDB and outcome events in a store.
type combined struct {
	metrics query.Querier
	events  query.Querier
}

func (c combined) QueryMetric(ctx context.Context, q query.Query) (query.Series, error) {
	return c.metrics.QueryMetric(ctx, q)
}
func (c combined) QueryEvents(ctx context.Context, q query.EventQuery) (query.EventGroups, error) {
	return c.events.QueryEvents(ctx, q)
}
func (c combined) Capabilities() query.Caps {
	m, e := c.metrics.Capabilities(), c.events.Capabilities()
	return query.Caps{
		Metrics:            m.Metrics,
		Events:             e.Events,
		MetricHistoryWeeks: m.MetricHistoryWeeks,
		EventHistoryWeeks:  e.EventHistoryWeeks,
	}
}
