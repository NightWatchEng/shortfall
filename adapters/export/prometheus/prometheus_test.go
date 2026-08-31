// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

var update = flag.Bool("update", false, "update golden files")

var at = time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)

func newExp(t *testing.T) (*Exporter, prometheus.Gatherer) {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	return e, e.Gatherer()
}

// sampleValue reads a single series' value out of the gathered families.
func sampleValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}

		if len(mf.Metric) != 1 {
			t.Fatalf("%s: want exactly 1 series, got %d", name, len(mf.Metric))
		}

		m := mf.Metric[0]
		if m.Counter != nil {
			return m.Counter.GetValue()
		}

		if m.Gauge != nil {
			return m.Gauge.GetValue()
		}

		t.Fatalf("%s: neither counter nor gauge", name)
	}

	t.Fatalf("%s: family not found", name)
	return 0
}

func TestExportMetricsMapping(t *testing.T) {
	cases := []struct {
		name    string
		points  []emit.MetricPoint
		wantErr bool
		family  string
		want    float64
	}{
		{
			name: "counter accumulates deltas",
			points: []emit.MetricPoint{
				{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
				{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 100, At: at},
			},
			family: "biz_value_total", want: 15000,
		},
		{
			name: "gauge takes the last level",
			points: []emit.MetricPoint{
				{Name: "biz_inflight_value", Labels: inflightLbls(), Value: 5568661, At: at},
				{Name: "biz_inflight_value", Labels: inflightLbls(), Value: 6000000, At: at},
			},
			family: "biz_inflight_value", want: 6000000,
		},
		{
			name: "txn counter counts",
			points: []emit.MetricPoint{
				{Name: "biz_txn_total", Labels: txnLbls(), Value: 3, At: at},
			},
			family: "biz_txn_total", want: 3,
		},
		{
			name: "provider counter counts",
			points: []emit.MetricPoint{
				{Name: "biz_provider_calls_total", Labels: map[string]string{"provider": "stripe", "op": "capture", "outcome": "failed"}, Value: 2, At: at},
			},
			family: "biz_provider_calls_total", want: 2,
		},
		{
			name:    "unknown family errors",
			points:  []emit.MetricPoint{{Name: "biz_bogus", Labels: map[string]string{}, Value: 1, At: at}},
			wantErr: true,
		},
		{
			name:    "negative counter delta errors",
			points:  []emit.MetricPoint{{Name: "biz_value_total", Labels: valueLbls("USD"), Value: -5, At: at}},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, g := newExp(t)
			err := e.ExportMetrics(context.Background(), c.points)
			if c.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := sampleValue(t, g, c.family); got != c.want {
				t.Fatalf("%s = %v, want %v", c.family, got, c.want)
			}
		})
	}
}

// TestInflightGaugeHonorsAtOrdering: biz_inflight_value is a level, and a
// stale sample (older At) arriving after a fresh one — which overlapping
// flushes can deliver, per emit's order-by-At contract — must not overwrite
// the fresh level. Counters are immune (Add commutes) and are not retested.
func TestInflightGaugeHonorsAtOrdering(t *testing.T) {
	older := at
	newer := at.Add(time.Minute)
	cases := []struct {
		name   string
		values []struct {
			v  int64
			at time.Time
		}
		want float64
	}{
		{
			name: "in-order keeps the latest",
			values: []struct {
				v  int64
				at time.Time
			}{{5568661, older}, {6000000, newer}},
			want: 6000000,
		},
		{
			name: "out-of-order ignores the stale sample",
			values: []struct {
				v  int64
				at time.Time
			}{{6000000, newer}, {5568661, older}},
			want: 6000000,
		},
		{
			name: "equal timestamps take the last",
			values: []struct {
				v  int64
				at time.Time
			}{{5568661, older}, {5568000, older}},
			want: 5568000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, g := newExp(t)
			for _, s := range c.values {
				if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
					{Name: "biz_inflight_value", Labels: inflightLbls(), Value: s.v, At: s.at},
				}); err != nil {
					t.Fatal(err)
				}
			}

			if got := sampleValue(t, g, "biz_inflight_value"); got != c.want {
				t.Fatalf("gauge = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCapabilitiesMetricsOnly(t *testing.T) {
	e, _ := newExp(t)
	c := e.Capabilities()
	if !c.Metrics || c.Events {
		t.Fatalf("caps = %+v, want Metrics-only", c)
	}
}

func TestExportEventsIsHonestNoop(t *testing.T) {
	e, g := newExp(t)
	err := e.ExportEvents(context.Background(), []biz.Outcome{
		{At: at, VC: biz.ValueContext{Flow: "invoice.pay", Money: biz.Money{Amount: 100, Currency: "USD", Exponent: 2}}, Stage: "capture", Result: biz.ResultFailed},
	})
	if err != nil {
		t.Fatalf("ExportEvents must not error: %v", err)
	}

	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}

	if len(mfs) != 0 {
		t.Fatalf("events must not create any series, got %d families", len(mfs))
	}
}

func TestDoubleRegisterErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(WithRegisterer(reg, reg)); err != nil {
		t.Fatal(err)
	}

	if _, err := New(WithRegisterer(reg, reg)); err == nil {
		t.Fatal("registering the same collectors twice must error, not panic")
	}
}

// TestMetricsGolden pins the exact /metrics exposition for a fixed input, so
// a change to family names, label order, help text, or type is a visible
// diff a human ratifies (run with -update to regenerate).
func TestMetricsGolden(t *testing.T) {
	e, g := newExp(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.ExportMetrics(ctx, []emit.MetricPoint{
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 100, At: at},
		{Name: "biz_value_total", Labels: valueLbls("EUR"), Value: 5000, At: at},
		{Name: "biz_txn_total", Labels: txnLbls(), Value: 3, At: at},
		{Name: "biz_inflight_value", Labels: inflightLbls(), Value: 5568661, At: at},
		{Name: "biz_inflight_value", Labels: inflightLbls(), Value: 6000000, At: at},
		{Name: "biz_provider_calls_total", Labels: map[string]string{"provider": "stripe", "op": "capture", "outcome": "failed"}, Value: 2, At: at},
	}))

	got := render(t, g)
	golden := filepath.Join("testdata", "metrics.golden")
	if *update {
		must(os.WriteFile(golden, got, 0o644))
	}

	want, err := os.ReadFile(golden)
	must(err)
	if !bytes.Equal(got, want) {
		t.Fatalf("/metrics does not match golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func render(t *testing.T, g prometheus.Gatherer) []byte {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}

	return buf.Bytes()
}

func valueLbls(cur string) map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": cur, "kind": "fee", "segment": "smb"}
}
func txnLbls() map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "segment": "smb"}
}
func inflightLbls() map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}
}
