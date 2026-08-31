// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package memq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/query"
)

var base = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func mp(name string, at time.Time, val int64, labels map[string]string) emit.MetricPoint {
	return emit.MetricPoint{Name: name, At: at, Value: val, Labels: labels}
}

func TestQueryMetricUnsupportedWhenNoMetrics(t *testing.T) {
	q := New(WithCaps(query.Caps{Events: true}))
	if _, err := q.QueryMetric(
		context.Background(),
		query.Query{Metric: "biz_txn_total"},
	); !errors.Is(err, query.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestQueryMetricAggregation(t *testing.T) {
	lbl := map[string]string{
		"flow":     "invoice.pay",
		"stage":    "capture",
		"outcome":  "failed",
		"currency": "USD",
	}
	metrics := []emit.MetricPoint{
		mp("biz_value_total", base.Add(1*time.Minute), 100, lbl),
		mp("biz_value_total", base.Add(2*time.Minute), 50, lbl),
		mp("biz_value_total", base.Add(90*time.Minute), 7, lbl), // outside a 1h range
		mp("biz_inflight_value", base.Add(1*time.Minute), 500, lbl),
		mp("biz_inflight_value", base.Add(2*time.Minute), 800, lbl), // later level wins
	}
	full := query.TimeRange{From: base, To: base.Add(time.Hour)}
	cases := []struct {
		name string
		q    query.Query
		want []float64 // per-point values, in order
	}{
		{
			name: "counter sums the increase over the whole range",
			q:    query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: full},
			want: []float64{150},
		},
		{
			name: "counter AggCount counts points",
			q:    query.Query{Metric: "biz_value_total", Agg: query.AggCount, Range: full},
			want: []float64{2},
		},
		{
			name: "gauge takes the last observed level",
			q:    query.Query{Metric: "biz_inflight_value", Range: full},
			want: []float64{800},
		},
		{
			name: "range excludes out-of-window points",
			q: query.Query{
				Metric: "biz_value_total",
				Agg:    query.AggSum,
				Range:  query.TimeRange{From: base, To: base.Add(3 * time.Minute)},
			},
			want: []float64{150},
		},
		{
			name: "step buckets the increase",
			q:    query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: full, Step: time.Minute},
			want: []float64{100, 50}, // minute 1 bucket, minute 2 bucket
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := New(WithMetrics(metrics))
			series, err := q.QueryMetric(context.Background(), c.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(series) != 1 {
				t.Fatalf("series = %d, want 1", len(series))
			}
			got := make([]float64, len(series[0].Points))
			for i, p := range series[0].Points {
				got[i] = p.Value
			}
			if len(got) != len(c.want) {
				t.Fatalf("points = %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("point[%d] = %v, want %v (all %v)", i, got[i], c.want[i], got)
				}
			}
		})
	}
}

// TestGaugeCarriesForward pins that a gauge level persists into later step
// buckets (and into the window from before it), matching a real backend's
// last_over_time — a bucket with no fresh sample must report the carried
// level, not a gap.
func TestGaugeCarriesForward(t *testing.T) {
	lbl := map[string]string{
		"flow":       "invoice.pay",
		"stage":      "capture",
		"age_bucket": "5m-30m",
		"currency":   "USD",
	}
	metrics := []emit.MetricPoint{
		mp("biz_inflight_value", base.Add(-time.Hour), 200, lbl), // set before the window
		mp("biz_inflight_value", base.Add(90*time.Second), 800, lbl),
	}
	q := New(WithMetrics(metrics))
	full := query.TimeRange{From: base, To: base.Add(5 * time.Minute)}
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_inflight_value",
		Range:  full,
		Step:   time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 {
		t.Fatalf("series = %d, want 1", len(series))
	}
	// 5 one-minute buckets. Bucket 0 (00:00-01:00): carries the pre-window
	// 200. Buckets 1-4: after the 800 sample at 01:30, all carry 800.
	got := make([]float64, len(series[0].Points))
	for i, p := range series[0].Points {
		got[i] = p.Value
	}
	want := []float64{200, 800, 800, 800, 800}
	if len(got) != len(want) {
		t.Fatalf("points = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bucket[%d] = %v, want %v (all %v)", i, got[i], want[i], got)
		}
	}
}

// TestGaugeSumsCollapsedSeries pins the gauge-aggregation regression: when
// GroupBy collapses several gauge series into one group, the bucket value
// must be the sum of each series' carried-forward level, matching a real
// backend's sum by(g)(last_over_time), not a single last sample across the
// whole group. Two stages (capture, settle) share one (age_bucket, currency)
// group; the answer is their sum.
func TestGaugeSumsCollapsedSeries(t *testing.T) {
	full := query.TimeRange{From: base, To: base.Add(5 * time.Minute)}
	metrics := []emit.MetricPoint{
		mp("biz_inflight_value", base.Add(time.Minute), 300, map[string]string{
			"flow":       "invoice.pay",
			"stage":      "capture",
			"age_bucket": "lt1m",
			"currency":   "USD",
		}),
		mp("biz_inflight_value", base.Add(time.Minute), 700, map[string]string{
			"flow":       "invoice.pay",
			"stage":      "settle",
			"age_bucket": "lt1m",
			"currency":   "USD",
		}),
	}
	q := New(WithMetrics(metrics))
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_inflight_value", Range: full, GroupBy: []string{"age_bucket", "currency"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 {
		t.Fatalf("series = %d, want 1 (one age_bucket/currency group)", len(series))
	}
	if got := series[0].Points[0].Value; got != 1000 {
		t.Fatalf("collapsed gauge value = %v, want 1000 (300 capture + 700 settle)", got)
	}
}

func TestQueryMetricGroupsByLabel(t *testing.T) {
	full := query.TimeRange{From: base, To: base.Add(time.Hour)}
	metrics := []emit.MetricPoint{
		mp("biz_txn_total", base.Add(time.Minute), 3, map[string]string{
			"flow":    "invoice.pay",
			"outcome": "failed",
		}),
		mp("biz_txn_total", base.Add(time.Minute), 1, map[string]string{
			"flow":    "invoice.pay",
			"outcome": "success",
		}),
	}
	q := New(WithMetrics(metrics))
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum, Range: full, GroupBy: []string{"outcome"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("series = %d, want 2 (failed, success)", len(series))
	}
	byOutcome := map[string]float64{}
	for _, s := range series {
		byOutcome[s.Labels["outcome"]] = s.Points[0].Value
	}
	if byOutcome["failed"] != 3 || byOutcome["success"] != 1 {
		t.Fatalf("grouped values = %v", byOutcome)
	}
}

func ev(at time.Time, flow, stage string, result biz.Result, amount int64, currency, customer, segment string) biz.Outcome {
	return biz.Outcome{
		At: at, Stage: stage, Result: result,
		VC: biz.ValueContext{
			Flow:       flow,
			CustomerID: customer,
			Segment:    segment,
			Money:      biz.Money{Amount: amount, Currency: currency, Exponent: 2},
			Kind:       biz.KindFee,
		},
	}
}

func TestQueryEventsGroupsSumsAndCurrencyInvariant(t *testing.T) {
	full := query.TimeRange{From: base, To: base.Add(time.Hour)}
	events := []biz.Outcome{
		ev(base.Add(1*time.Minute), "invoice.pay", "capture", biz.ResultFailed, 14900, "USD", "h:c1", "smb"),
		ev(base.Add(2*time.Minute), "invoice.pay", "capture", biz.ResultFailed, 100, "USD", "h:c1", "smb"),
		ev(base.Add(3*time.Minute), "invoice.pay", "capture", biz.ResultFailed, 5000, "EUR", "h:c2", "enterprise"),
	}
	q := New(WithEvents(events))
	ctx := context.Background()

	t.Run("sum without currency grouped or pinned is refused", func(t *testing.T) {
		_, err := q.QueryEvents(ctx, query.EventQuery{Range: full, GroupBy: []string{"customer"}})
		if err == nil {
			t.Fatal("cross-currency sum must be refused")
		}
	})

	t.Run("grouping by currency yields per-currency sums", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   full,
			GroupBy: []string{"currency"},
			OrderBy: query.OrderSumDesc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 2 {
			t.Fatalf("groups = %d, want 2", len(groups))
		}
		if groups[0].Key["currency"] != "USD" || groups[0].SumMinor != 15000 || groups[0].Count != 2 {
			t.Fatalf("top group = %+v, want USD sum 15000 count 2", groups[0])
		}
		if groups[1].Key["currency"] != "EUR" || groups[1].SumMinor != 5000 {
			t.Fatalf("second group = %+v", groups[1])
		}
	})

	t.Run("pinning currency via filter allows a single-currency sum", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   full,
			Filters: map[string]string{"currency": "USD"},
			GroupBy: []string{"customer"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].SumMinor != 15000 {
			t.Fatalf("groups = %+v", groups)
		}
	})

	t.Run("max_per_group sets MaxMinor to the largest event, not the sum (ADR-0009)", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   full,
			Filters: map[string]string{"currency": "USD"},
			GroupBy: []string{"customer"},
			Agg:     query.EventAggMaxPerGroup,
		})
		if err != nil {
			t.Fatal(err)
		}
		// h:c1 has two USD failures (14900, 100): Count 2, SumMinor 15000, MaxMinor 14900.
		if len(groups) != 1 || groups[0].Count != 2 || groups[0].SumMinor != 15000 ||
			groups[0].MaxMinor != 14900 {
			t.Fatalf("group = %+v, want count 2 sum 15000 max 14900", groups[0])
		}
	})

	t.Run("max_per_group still enforces the currency invariant", func(t *testing.T) {
		_, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   full,
			GroupBy: []string{"customer"},
			Agg:     query.EventAggMaxPerGroup,
		})
		if err == nil {
			t.Fatal("max across currencies must be refused, like a cross-currency sum")
		}
	})

	t.Run("max seed is sign-independent (parity must not assume non-negative)", func(t *testing.T) {
		// memq.WithEvents does not validate money, so the running max must not
		// seed at 0: an all-negative group must report its real (negative) max,
		// exactly as SQL MAX() would, not 0.
		neg := []biz.Outcome{
			{At: base.Add(time.Minute), Stage: "capture", Result: biz.ResultFailed,
				VC: biz.ValueContext{
					Flow:       "invoice.pay",
					CustomerID: "h:x",
					Money:      biz.Money{Amount: -300, Currency: "USD", Exponent: 2},
				}},
			{At: base.Add(2 * time.Minute), Stage: "capture", Result: biz.ResultFailed,
				VC: biz.ValueContext{
					Flow:       "invoice.pay",
					CustomerID: "h:x",
					Money:      biz.Money{Amount: -100, Currency: "USD", Exponent: 2},
				}},
		}
		qn := New(WithEvents(neg))
		groups, err := qn.QueryEvents(ctx, query.EventQuery{
			Range:   full,
			Filters: map[string]string{"currency": "USD"},
			GroupBy: []string{"customer"},
			Agg:     query.EventAggMaxPerGroup,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].MaxMinor != -100 {
			t.Fatalf("group = %+v, want MaxMinor -100 (not the 0 seed)", groups[0])
		}
	})

	t.Run("EventAggGroups leaves MaxMinor zero (populated only for max_per_group)", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range:   full,
			Filters: map[string]string{"currency": "USD"},
			GroupBy: []string{"customer"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if groups[0].MaxMinor != 0 {
			t.Fatalf("MaxMinor = %d, want 0 for EventAggGroups", groups[0].MaxMinor)
		}
	})
}

func TestQueryEventsOrderLimitAndDistinct(t *testing.T) {
	full := query.TimeRange{From: base, To: base.Add(time.Hour)}
	events := []biz.Outcome{
		ev(base.Add(1*time.Minute), "invoice.pay", "capture", biz.ResultFailed, 100, "USD", "h:c1", "smb"),
		ev(base.Add(2*time.Minute), "invoice.pay", "capture", biz.ResultFailed, 900, "USD", "h:c2", "smb"),
		ev(base.Add(3*time.Minute), "invoice.pay", "capture", biz.ResultFailed, 50, "USD", "h:c2", "smb"),
	}
	q := New(WithEvents(events))
	ctx := context.Background()

	t.Run("sum_desc with limit keeps the top account", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range: full, Filters: map[string]string{"currency": "USD"},
			GroupBy: []string{"customer"}, OrderBy: query.OrderSumDesc, Limit: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Key["customer"] != "h:c2" || groups[0].SumMinor != 950 {
			t.Fatalf("top = %+v, want h:c2 sum 950", groups)
		}
	})

	t.Run("limit without order is an error", func(t *testing.T) {
		_, err := q.QueryEvents(ctx, query.EventQuery{
			Range: full, Filters: map[string]string{"currency": "USD"},
			GroupBy: []string{"customer"}, Limit: 1,
		})
		if err == nil {
			t.Fatal("Limit without OrderBy must error")
		}
	})

	t.Run("distinct_count counts distinct customers without summing", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range: full, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Count != 2 {
			t.Fatalf("distinct = %+v, want count 2", groups)
		}
	})
}

func TestQueryEventsUnsupportedWhenNoEvents(t *testing.T) {
	q := New(WithCaps(query.Caps{Metrics: true}))
	if _, err := q.QueryEvents(
		context.Background(),
		query.EventQuery{},
	); !errors.Is(err, query.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
