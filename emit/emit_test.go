package emit

import (
	"context"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
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
func (f *fakeExporter) Capabilities() Caps             { return Caps{Events: true, HistoryWeeks: 8} }
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
