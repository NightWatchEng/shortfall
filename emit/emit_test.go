// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"context"
	"reflect"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/query"
)

// fakeExporter proves the frozen Exporter surface is implementable — the
// freeze test is that this compiles and behaves; adapters implement the
// same shape out of tree.
type fakeExporter struct {
	metrics int
	events  int
	closed  bool
}

func (f *fakeExporter) ExportMetrics(_ context.Context, batch []MetricPoint) error {
	f.metrics += len(batch)
	return nil
}
func (f *fakeExporter) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	f.events += len(batch)
	return nil
}
func (f *fakeExporter) Capabilities() Caps {
	return Caps{Metrics: true, Events: true, MetricHistoryWeeks: 2, EventHistoryWeeks: 8}
}
func (f *fakeExporter) Shutdown(context.Context) error { f.closed = true; return nil }

var _ Exporter = (*fakeExporter)(nil)

func TestFrozenExporterSurface(t *testing.T) {
	cases := []struct {
		name    string
		metrics []MetricPoint
		events  []biz.Outcome
		wantM   int
		wantE   int
	}{
		{"empty batches", nil, nil, 0, 0},
		{"one of each", []MetricPoint{{Name: "biz_txn_total", Value: 1}}, make([]biz.Outcome, 1), 1, 1},
		{"metric batch", []MetricPoint{{}, {}, {}}, nil, 3, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeExporter{}
			if err := f.ExportMetrics(context.Background(), c.metrics); err != nil {
				t.Fatal(err)
			}

			if err := f.ExportEvents(context.Background(), c.events); err != nil {
				t.Fatal(err)
			}

			if f.metrics != c.wantM || f.events != c.wantE {
				t.Fatalf("got %d/%d, want %d/%d", f.metrics, f.events, c.wantM, c.wantE)
			}

			if err := f.Shutdown(context.Background()); err != nil || !f.closed {
				t.Fatal("shutdown contract broken")
			}
		})
	}
}

func TestOptionsWriteIntoRecordConfig(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
		want RecordConfig
	}{
		{"source", func(c *RecordConfig) { c.Source = "stripe:webhook" }, RecordConfig{Source: "stripe:webhook"}},
		{"err", func(c *RecordConfig) { c.Err = "timeout" }, RecordConfig{Err: "timeout"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cfg RecordConfig
			c.opt(&cfg)
			if cfg != c.want {
				t.Fatalf("got %+v, want %+v", cfg, c.want)
			}
		})
	}
}

// TestCapsMirrorsQueryCaps pins the deliberate emit/query Caps mirror:
// the two shapes must be amended together or capability honesty drifts
// between the write and read sides.
func TestCapsMirrorsQueryCaps(t *testing.T) {
	a, b := reflect.TypeOf(Caps{}), reflect.TypeOf(query.Caps{})
	if a.NumField() != b.NumField() {
		t.Fatalf("emit.Caps has %d fields, query.Caps has %d — amend both together", a.NumField(), b.NumField())
	}

	for i := 0; i < a.NumField(); i++ {
		af, bf := a.Field(i), b.Field(i)
		if af.Name != bf.Name || af.Type != bf.Type {
			t.Fatalf("Caps field %d drifted: emit %s %v vs query %s %v", i, af.Name, af.Type, bf.Name, bf.Type)
		}
	}
}
