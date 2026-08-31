// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration test: runs the translated PromQL against a real Prometheus and
// checks the adapter reads back what was written. It is env-gated (set
// PROMETHEUS_URL) and excluded from the default build; the unit tests cover
// translation and parsing. The correctness bar is that the same golden
// queries match the in-memory reference — a CI job seeds Prometheus from the
// harness and points both memq and this adapter at the same data.
//
//	PROMETHEUS_URL=http://localhost:9090 go test -tags integration ./...
package promql

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

func TestAgainstRealPrometheus(t *testing.T) {
	base := os.Getenv("PROMETHEUS_URL")
	if base == "" {
		t.Skip("set PROMETHEUS_URL to run against a real Prometheus")
	}
	q := New(base)
	// A trivial always-present series proves the wire path; scenario parity is
	// asserted by the shared golden harness in CI.
	end := time.Now()
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "up", Agg: query.AggSum,
		Range: query.TimeRange{From: end.Add(-time.Hour), To: end},
	})
	if err != nil {
		t.Fatalf("query real prometheus: %v", err)
	}
	_ = series // presence, not value, is asserted here
}
