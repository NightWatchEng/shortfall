// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeQuerier proves the frozen Querier surface is implementable,
// including the honest metrics-only path.
type fakeQuerier struct {
	metrics bool
	events  bool
}

func (f fakeQuerier) QueryMetric(_ context.Context, q Query) (Series, error) {
	if !f.metrics {
		return nil, ErrUnsupported
	}

	return Series{{Labels: q.Filters, Points: []Point{{At: q.Range.From, Value: 1}}}}, nil
}
func (f fakeQuerier) QueryEvents(context.Context, EventQuery) (EventGroups, error) {
	if !f.events {
		return nil, ErrUnsupported
	}

	return EventGroups{{Count: 1, SumMinor: 14900}}, nil
}
func (f fakeQuerier) Capabilities() Caps {
	return Caps{Metrics: f.metrics, Events: f.events, MetricHistoryWeeks: 2, EventHistoryWeeks: 8}
}

var _ Querier = fakeQuerier{}

func TestFrozenQuerierSurface(t *testing.T) {
	window := TimeRange{From: time.Unix(1000, 0), To: time.Unix(2000, 0)}
	cases := []struct {
		name         string
		q            fakeQuerier
		wantMetricEr error
		wantEventsEr error
		wantGroups   int
	}{
		{"full backend", fakeQuerier{metrics: true, events: true}, nil, nil, 1},
		{"metrics-only refuses events honestly", fakeQuerier{metrics: true}, nil, ErrUnsupported, 0},
		{"events-only refuses metrics honestly", fakeQuerier{events: true}, ErrUnsupported, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.q.QueryMetric(context.Background(), Query{Metric: "biz_txn_total", Agg: AggSum, Range: window})
			if !errors.Is(err, c.wantMetricEr) {
				t.Fatalf("metric err = %v, want %v", err, c.wantMetricEr)
			}

			groups, err := c.q.QueryEvents(context.Background(), EventQuery{
				Range:   window,
				GroupBy: []string{"currency", "customer"},
				OrderBy: OrderSumDesc,
				Limit:   20,
			})
			if !errors.Is(err, c.wantEventsEr) {
				t.Fatalf("events err = %v, want %v", err, c.wantEventsEr)
			}

			if len(groups) != c.wantGroups {
				t.Fatalf("groups = %d, want %d", len(groups), c.wantGroups)
			}

			caps := c.q.Capabilities()
			if caps.Metrics != c.q.metrics || caps.Events != c.q.events {
				t.Fatalf("capability honesty broken: %+v", caps)
			}
		})
	}
}

func TestEventAggAndOrderVocabulary(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"groups default", string(EventAggGroups), ""},
		{"distinct count", string(EventAggDistinctCount), "distinct_count"},
		{"order none", string(OrderNone), ""},
		{"order sum desc", string(OrderSumDesc), "sum_desc"},
		{"order count desc", string(OrderCountDesc), "count_desc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Fatalf("got %q, want %q", c.got, c.want)
			}
		})
	}
}
