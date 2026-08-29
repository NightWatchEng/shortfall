package otlp

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

var at = time.Date(2026, 8, 27, 14, 32, 0, 0, time.UTC)

// fakeMetric / fakeLog capture what the adapter would ship.
type fakeMetric struct {
	got      *metricdata.ResourceMetrics
	fail     bool
	shutErr  error
	shutdown bool
}

func (f *fakeMetric) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	if f.fail {
		return errors.New("metric backend down")
	}
	f.got = rm
	return nil
}
func (f *fakeMetric) Shutdown(context.Context) error { f.shutdown = true; return f.shutErr }

type fakeLog struct {
	got      []biz.Outcome
	fail     bool
	shutErr  error
	shutdown bool
}

func (f *fakeLog) emit(_ context.Context, batch []biz.Outcome) error {
	if f.fail {
		return errors.New("log backend down")
	}
	f.got = append(f.got, batch...)
	return nil
}
func (f *fakeLog) Shutdown(context.Context) error { f.shutdown = true; return f.shutErr }

func vc() biz.ValueContext {
	return biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_1", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
}

func TestBuildResourceMetricsFamilyKinds(t *testing.T) {
	batch := []emit.MetricPoint{
		{Name: "biz_value_total", Labels: map[string]string{"flow": "invoice.pay"}, Value: 14900, At: at},
		{Name: "biz_txn_total", Labels: map[string]string{"flow": "invoice.pay"}, Value: 1, At: at},
		{Name: "biz_inflight_value", Labels: map[string]string{"flow": "invoice.pay"}, Value: 500, At: at},
	}
	rm, err := buildResourceMetrics(batch, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rm.ScopeMetrics) != 1 {
		t.Fatalf("scope metrics: %d", len(rm.ScopeMetrics))
	}
	kinds := map[string]string{}
	var stamped bool
	for _, m := range rm.ScopeMetrics[0].Metrics {
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			kinds[m.Name] = "sum"
			if d.Temporality != metricdata.DeltaTemporality || !d.IsMonotonic {
				t.Fatalf("%s must be a delta monotonic sum", m.Name)
			}
			if d.DataPoints[0].Time.Equal(at) {
				stamped = true
			}
		case metricdata.Gauge[int64]:
			kinds[m.Name] = "gauge"
		default:
			t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
		}
	}
	cases := []struct{ name, want string }{
		{"biz_value_total", "sum"},
		{"biz_txn_total", "sum"},
		{"biz_inflight_value", "gauge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if kinds[c.name] != c.want {
				t.Fatalf("%s mapped to %q, want %q", c.name, kinds[c.name], c.want)
			}
		})
	}
	if !stamped {
		t.Fatal("data points must keep the point's own observation time, not flush time")
	}
}

func TestBuildRecordCarriesMoneyOnEventsOnly(t *testing.T) {
	o := biz.Outcome{
		At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed,
		Source: "stripe:webhook",
	}
	r := buildRecord(o)
	if r.EventName() != eventName {
		t.Fatalf("event name %q", r.EventName())
	}
	if !r.Timestamp().Equal(at) {
		t.Fatalf("timestamp %v, want %v", r.Timestamp(), at)
	}
	attrs := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value.String()
		return true
	})
	want := map[string]string{
		"biz.flow": "invoice.pay", "biz.stage": "capture", "biz.outcome": "failed",
		"biz.entity.id": "inv_1", "biz.customer.id": "h:c", "biz.currency": "USD",
		"biz.value.kind": "fee", "source": "stripe:webhook", "biz.amount_minor": "14900",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Fatalf("attr %s = %q, want %q (all %v)", k, attrs[k], v, attrs)
		}
	}
}

func TestExporterShipsAndReportsCapabilities(t *testing.T) {
	fm, fl := &fakeMetric{}, &fakeLog{}
	e := newWith(fm, fl)
	if c := e.Capabilities(); !c.Metrics || !c.Events {
		t.Fatalf("caps: %+v", c)
	}
	ctx := context.Background()
	if err := e.ExportMetrics(ctx, []emit.MetricPoint{{Name: "biz_txn_total", Value: 1, At: at}}); err != nil {
		t.Fatal(err)
	}
	if err := e.ExportEvents(ctx, []biz.Outcome{{At: at, VC: vc(), Stage: "auth", Result: biz.ResultSuccess}}); err != nil {
		t.Fatal(err)
	}
	if fm.got == nil || len(fl.got) != 1 {
		t.Fatalf("nothing shipped: metrics=%v events=%d", fm.got, len(fl.got))
	}
}

func TestExporterEmptyBatchesNoop(t *testing.T) {
	fm, fl := &fakeMetric{}, &fakeLog{}
	e := newWith(fm, fl)
	if err := e.ExportMetrics(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ExportEvents(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if fm.got != nil || len(fl.got) != 0 {
		t.Fatal("empty batch must not call the backend")
	}
}

func TestExporterSurfacesFailuresAndShutdown(t *testing.T) {
	cases := []struct {
		name string
		fm   *fakeMetric
		fl   *fakeLog
		do   func(e *Exporter) error
	}{
		{"metric failure", &fakeMetric{fail: true}, &fakeLog{}, func(e *Exporter) error {
			return e.ExportMetrics(context.Background(), []emit.MetricPoint{{Name: "biz_txn_total", Value: 1, At: at}})
		}},
		{"event failure", &fakeMetric{}, &fakeLog{fail: true}, func(e *Exporter) error {
			return e.ExportEvents(context.Background(), []biz.Outcome{{At: at, VC: vc(), Stage: "auth", Result: biz.ResultSuccess}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWith(c.fm, c.fl)
			if err := c.do(e); err == nil {
				t.Fatal("export failure must surface")
			}
		})
	}
	fm, fl := &fakeMetric{}, &fakeLog{}
	e := newWith(fm, fl)
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fm.shutdown || !fl.shutdown {
		t.Fatal("Shutdown must close both exporters")
	}
}

// A shutdown failure on either leg must surface, and when both legs fail
// neither error may be masked — a swallowed log-leg flush error can hide
// dropped outcome data.
func TestShutdownJoinsBothErrors(t *testing.T) {
	mFail := errors.New("metric backend down")
	lFail := errors.New("log backend down")
	cases := []struct {
		name       string
		fm         *fakeMetric
		fl         *fakeLog
		wantMetric bool
		wantEvent  bool
		wantCloseM bool
		wantCloseL bool
	}{
		{"both fail", &fakeMetric{shutErr: mFail}, &fakeLog{shutErr: lFail}, true, true, true, true},
		{"metric only", &fakeMetric{shutErr: mFail}, &fakeLog{}, true, false, true, true},
		{"event only", &fakeMetric{}, &fakeLog{shutErr: lFail}, false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newWith(c.fm, c.fl)
			err := e.Shutdown(context.Background())
			if err == nil {
				t.Fatal("Shutdown must surface a leg failure")
			}
			if errors.Is(err, mFail) != c.wantMetric {
				t.Fatalf("metric error surfaced=%v, want %v (err=%v)", errors.Is(err, mFail), c.wantMetric, err)
			}
			if errors.Is(err, lFail) != c.wantEvent {
				t.Fatalf("event error surfaced=%v, want %v (err=%v)", errors.Is(err, lFail), c.wantEvent, err)
			}
			if c.fm.shutdown != c.wantCloseM || c.fl.shutdown != c.wantCloseL {
				t.Fatal("both legs must be closed even when one fails")
			}
		})
	}
}
