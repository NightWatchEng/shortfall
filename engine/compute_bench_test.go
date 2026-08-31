// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
)

// benchWindow is a 24h incident window (the deferred snapshot lands at To).
var benchWindow = query.TimeRange{
	From: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
	To:   time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC),
}

// buildIncident synthesizes an incident-scale dataset: nEvents outcome events
// spread across the window over many customers and a few currencies (a realistic
// mix of failed/success so realized de-dup, recovery exclusion, and the
// customers grouping all do real work), plus in-flight gauge points across
// every age bucket for the deferred leg.
func buildIncident(nEvents int) *memq.Querier {
	currencies := []string{"USD", "EUR", "GBP"}
	buckets := emit.AgeBuckets
	events := make([]biz.Outcome, 0, nEvents)
	span := benchWindow.To.Sub(benchWindow.From)
	// Per-event stride computed before multiplying by i: i*span would overflow
	// int64 (span is ~8.6e13 ns) past ~107k events and wrap negative, silently
	// dropping events out of the window.
	stride := span / time.Duration(nEvents)
	for i := 0; i < nEvents; i++ {
		cur := currencies[i%len(currencies)]
		result := biz.ResultFailed
		if i%3 == 0 {
			result = biz.ResultSuccess // a third recover / succeed
		}
		events = append(events, biz.Outcome{
			At:    benchWindow.From.Add(time.Duration(i) * stride),
			Stage: "capture", Result: result,
			VC: biz.ValueContext{
				Flow:       "invoice.pay",
				EntityID:   fmt.Sprintf("inv_%d", i),
				CustomerID: fmt.Sprintf("h:c%06d", i%50000), // 50k distinct accounts
				Segment:    []string{"smb", "enterprise"}[i%2],
				Money:      biz.Money{Amount: int64(100 + i%9000), Currency: cur, Exponent: 2},
				Kind:       biz.KindFee,
			},
		})
	}
	// In-flight gauge levels: one per (stage, bucket, currency) at the snapshot.
	var metrics []emit.MetricPoint
	for _, stage := range []string{"capture", "settle"} {
		for _, b := range buckets {
			for _, cur := range currencies {
				metrics = append(metrics, emit.MetricPoint{
					Name:   "biz_inflight_value",
					Labels: map[string]string{"flow": "invoice.pay", "stage": stage, "age_bucket": b, "currency": cur},
					Value:  int64(1_000_000 + len(metrics)*777),
					At:     benchWindow.To.Add(-time.Minute),
				})
			}
		}
	}
	return memq.New(memq.WithEvents(events), memq.WithMetrics(metrics))
}

// BenchmarkCompute measures the full four-leg assembly at incident scale
// through the in-memory querier — the code path an on-call human waits on
// during a Sev1. Reported per event scale so benchstat tracks ns/op and
// allocs/op as the dataset grows.
//
// Documented baseline (Apple M-series, Go 1.25, this dataset shape; absolute
// numbers vary by host — the gate compares PR vs main, not against these):
//
//	Compute/events=50000    ~200 ms/op   ~164 MB/op   ~2.3M allocs/op
//	Compute/events=200000   ~730 ms/op   ~646 MB/op   ~10M  allocs/op
//
// Time, memory, and allocations grow roughly linearly with event count (each
// leg is a bounded number of full passes and the memq grouping builds per-group
// maps) — that linearity is the property the gate should hold. ~1M events
// extrapolates to ~3 GB, so the checked-in sizes stay CI-safe.
func BenchmarkCompute(b *testing.B) {
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		b.Fatal(err)
	}
	req := Request{Window: benchWindow, Flows: []string{"invoice.pay"}}
	for _, n := range []int{50_000, 200_000} {
		q := buildIncident(n)
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				rep, err := Compute(context.Background(), &reg, q, req)
				if err != nil {
					b.Fatal(err)
				}
				if len(rep.Realized.ByCurrency) == 0 {
					b.Fatal("benchmark computed an empty realized leg — dataset wrong")
				}
			}
		})
	}
}
