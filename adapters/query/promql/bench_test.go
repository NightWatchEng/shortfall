// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package promql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

// BenchmarkSteppedQuery measures a stepped QueryMetric over real localhost
// HTTP round trips — the engine's baseline path issues hundreds of these
// (Step=1h over multi-week lookbacks), so per-bucket latency dominates.
func BenchmarkSteppedQuery(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"currency":"USD"},"value":[1,"100"]}
		]}}`))
	}))
	defer srv.Close()

	q := New(srv.URL)
	from := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	req := query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum,
		Filters: map[string]string{"flow": "invoice.pay"},
		GroupBy: []string{"currency"},
		Range:   query.TimeRange{From: from, To: from.Add(64 * time.Hour)},
		Step:    time.Hour, // 64 buckets, one instant query each
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := q.QueryMetric(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// rttDoer adds a fixed simulated round-trip time to every request — the
// cost model the fan-out exists for: against a real backend each bucket
// pays a network RTT, and the localhost benchmark above cannot show that.
type rttDoer struct {
	inner Doer
	rtt   time.Duration
}

func (r rttDoer) Do(req *http.Request) (*http.Response, error) {
	time.Sleep(r.rtt)
	return r.inner.Do(req)
}

// BenchmarkSteppedQueryRTT5ms is BenchmarkSteppedQuery with a simulated 5ms
// RTT per request: 64 buckets sequentially would floor at ~320ms; the
// bounded fan-out divides that by its concurrency.
func BenchmarkSteppedQueryRTT5ms(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"currency":"USD"},"value":[1,"100"]}
		]}}`))
	}))
	defer srv.Close()

	q := New(srv.URL, WithHTTPClient(rttDoer{inner: srv.Client(), rtt: 5 * time.Millisecond}))
	from := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	req := query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum,
		Filters: map[string]string{"flow": "invoice.pay"},
		GroupBy: []string{"currency"},
		Range:   query.TimeRange{From: from, To: from.Add(64 * time.Hour)},
		Step:    time.Hour,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := q.QueryMetric(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}
