// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"errors"
	"testing"
	"time"
)

// capsQuerier is a fakeQuerier with distinguishable retentions, so a test
// can tell which side each Caps field came from.
type capsQuerier struct {
	fakeQuerier
	metricWeeks, eventWeeks int
}

func (c capsQuerier) Capabilities() Caps {
	return Caps{
		Metrics:            c.metrics,
		Events:             c.events,
		MetricHistoryWeeks: c.metricWeeks,
		EventHistoryWeeks:  c.eventWeeks,
	}
}

func TestCombineRoutesEachVerbToItsBackend(t *testing.T) {
	window := TimeRange{From: time.Unix(1000, 0), To: time.Unix(2000, 0)}
	metrics := capsQuerier{fakeQuerier: fakeQuerier{metrics: true}, metricWeeks: 12, eventWeeks: 99}
	events := capsQuerier{fakeQuerier: fakeQuerier{events: true}, metricWeeks: 99, eventWeeks: 8}

	q := Combine(metrics, events)

	series, err := q.QueryMetric(context.Background(), Query{Metric: "biz_txn_total", Agg: AggSum, Range: window})
	if err != nil || len(series) != 1 {
		t.Fatalf("QueryMetric via the metrics side: series=%v err=%v", series, err)
	}

	groups, err := q.QueryEvents(context.Background(), EventQuery{Range: window})
	if err != nil || len(groups) != 1 {
		t.Fatalf("QueryEvents via the events side: groups=%v err=%v", groups, err)
	}

	// Each Caps field comes from the backend that owns the signal — the
	// events side's opinion of metric retention is noise, and vice versa.
	want := Caps{Metrics: true, Events: true, MetricHistoryWeeks: 12, EventHistoryWeeks: 8}
	if got := q.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}

func TestCombineDoesNotCrossRoute(t *testing.T) {
	// Both sides refuse the other's verb; Combine must never fall back to
	// the wrong side, or an events-only store would be asked for metrics.
	metrics := fakeQuerier{metrics: true}
	events := fakeQuerier{events: true}
	q := Combine(events, metrics) // deliberately swapped

	if _, err := q.QueryMetric(context.Background(), Query{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("metric verb must reach the first argument only; err = %v", err)
	}

	if _, err := q.QueryEvents(context.Background(), EventQuery{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("event verb must reach the second argument only; err = %v", err)
	}

	if got := q.Capabilities(); got.Metrics || got.Events {
		t.Fatalf("Capabilities() = %+v: a swapped pairing must declare neither signal", got)
	}
}

func TestCombineRejectsNilSides(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Combine(nil, nil) must panic: a nil side is a wiring bug, not a backend")
		}
	}()

	Combine(nil, nil)
}
