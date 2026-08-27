package emit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

// captureExporter records everything it is handed, with an optional
// failure mode — the emitter's observable output.
type captureExporter struct {
	mu      sync.Mutex
	metrics []MetricPoint
	events  []biz.Outcome
	failAll bool
	closed  bool
}

func (c *captureExporter) ExportMetrics(_ context.Context, batch []MetricPoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failAll {
		return errors.New("backend down")
	}
	c.metrics = append(c.metrics, batch...)
	return nil
}
func (c *captureExporter) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failAll {
		return errors.New("backend down")
	}
	c.events = append(c.events, batch...)
	return nil
}
func (c *captureExporter) Capabilities() Caps {
	return Caps{Metrics: true, Events: true}
}
func (c *captureExporter) Shutdown(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *captureExporter) snapshot() ([]MetricPoint, []biz.Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := append([]MetricPoint(nil), c.metrics...)
	e := append([]biz.Outcome(nil), c.events...)
	return m, e
}

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

var testClock = time.Date(2026, 8, 27, 14, 30, 0, 0, time.UTC)

func newTestEmitter(t *testing.T, exp Exporter) *Std {
	t.Helper()
	em, err := New(testRegistry(t), exp, WithClock(func() time.Time { return testClock }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	return em
}

func ctxWithVC(t *testing.T, vc biz.ValueContext) context.Context {
	t.Helper()
	ctx, err := biz.WithValueContext(context.Background(), vc)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func emitterVC() biz.ValueContext {
	return biz.ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_001",
		CustomerID: "h:c1",
		Segment:    "smb",
		Money:      biz.Money{Amount: 14900, Currency: "USD", Exponent: 2},
		Kind:       biz.KindFee,
	}
}

// flushAndSnapshot forces a synchronous flush so tests never sleep.
func flushAndSnapshot(t *testing.T, em *Std, exp *captureExporter) ([]MetricPoint, []biz.Outcome) {
	t.Helper()
	em.Flush(context.Background())
	return exp.snapshot()
}

func metricsByName(points []MetricPoint) map[string][]MetricPoint {
	out := map[string][]MetricPoint{}
	for _, p := range points {
		out[p.Name] = append(out[p.Name], p)
	}
	return out
}

func TestRecordHappyPath(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed, WithSource("httpmw"))

	metrics, events := flushAndSnapshot(t, em, exp)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"stage", ev.Stage, "capture"},
		{"result", ev.Result, biz.ResultFailed},
		{"source", ev.Source, "httpmw"},
		{"at stamped by clock", ev.At, testClock},
		{"entity kept raw on the event", ev.VC.EntityID, "inv_001"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Fatalf("got %v, want %v", c.got, c.want)
			}
		})
	}

	byName := metricsByName(metrics)
	value := byName["biz_value_total"]
	txn := byName["biz_txn_total"]
	if len(value) != 1 || len(txn) != 1 {
		t.Fatalf("metric families: %d value, %d txn (want 1 each); all=%v", len(value), len(txn), metrics)
	}
	labelCases := []struct {
		name   string
		labels map[string]string
		want   map[string]string
	}{
		{"value labels", value[0].Labels, map[string]string{
			"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
			"currency": "USD", "kind": "fee", "segment": "smb",
		}},
		{"txn labels", txn[0].Labels, map[string]string{
			"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
			"currency": "USD", "segment": "smb",
		}},
	}
	for _, c := range labelCases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.labels) != len(c.want) {
				t.Fatalf("label set %v, want exactly %v", c.labels, c.want)
			}
			for k, v := range c.want {
				if c.labels[k] != v {
					t.Fatalf("label %s = %q, want %q", k, c.labels[k], v)
				}
			}
		})
	}
	if value[0].Value != 14900 || txn[0].Value != 1 {
		t.Fatalf("values %d/%d, want 14900/1", value[0].Value, txn[0].Value)
	}
	if !value[0].At.Equal(testClock) {
		t.Fatalf("metric At %v, want clock time", value[0].At)
	}
}

func TestRecordDropsAreLoudNeverSilent(t *testing.T) {
	cases := []struct {
		name       string
		ctx        func(t *testing.T) context.Context
		wantReason string
	}{
		{"absent context", func(t *testing.T) context.Context { return context.Background() }, "invalid"},
		{"pii in entity", func(t *testing.T) context.Context {
			vc := emitterVC()
			vc.EntityID = "4111111111111111"
			// bypass WithValueContext validation-free encode: encode does
			// not Validate, so the PII arrives at Record — which must be
			// the fence.
			return ctxWithVC(t, vc)
		}, "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &captureExporter{}
			em := newTestEmitter(t, exp)
			em.Record(c.ctx(t), "capture", biz.ResultFailed)
			metrics, events := flushAndSnapshot(t, em, exp)
			if len(events) != 0 {
				t.Fatalf("dropped outcome still exported: %v", events)
			}
			drops := metricsByName(metrics)["biz_dropped_events_total"]
			if len(drops) != 1 || drops[0].Labels["reason"] != c.wantReason || drops[0].Value != 1 {
				t.Fatalf("drop counter = %v, want one %s", drops, c.wantReason)
			}
		})
	}
}

func TestLabelFallbacks(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*biz.ValueContext)
		wantFlow    string
		wantSegment string
	}{
		{"unregistered flow", func(vc *biz.ValueContext) { vc.Flow = "mystery.flow" }, "unregistered", "smb"},
		{"segment outside enum", func(vc *biz.ValueContext) { vc.Segment = "gov" }, "invoice.pay", ""},
		{"empty segment stays empty", func(vc *biz.ValueContext) { vc.Segment = "" }, "invoice.pay", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &captureExporter{}
			em := newTestEmitter(t, exp)
			vc := emitterVC()
			c.mutate(&vc)
			em.Record(ctxWithVC(t, vc), "capture", biz.ResultSuccess)
			metrics, events := flushAndSnapshot(t, em, exp)
			value := metricsByName(metrics)["biz_value_total"]
			if len(value) != 1 {
				t.Fatalf("value points: %v", metrics)
			}
			if got := value[0].Labels["flow"]; got != c.wantFlow {
				t.Fatalf("flow label %q, want %q", got, c.wantFlow)
			}
			if got := value[0].Labels["segment"]; got != c.wantSegment {
				t.Fatalf("segment label %q, want %q", got, c.wantSegment)
			}
			// The event always keeps the raw truth for diagnosis.
			if len(events) != 1 || events[0].VC.Flow == "unregistered" {
				t.Fatalf("event must keep raw flow: %v", events)
			}
		})
	}
}

func TestUnregisteredStageFallsBack(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.Record(ctxWithVC(t, emitterVC()), "refund", biz.ResultSuccess)
	metrics, _ := flushAndSnapshot(t, em, exp)
	value := metricsByName(metrics)["biz_value_total"]
	if len(value) != 1 || value[0].Labels["stage"] != "unregistered" {
		t.Fatalf("stage fallback: %v", value)
	}
}

func TestDedupKeyIncludesResult(t *testing.T) {
	// DEVIATION from the proposal's (flow, entity, stage) key, on
	// purpose: the engine's realized leg de-duplicates failures against
	// LATER SUCCESSES for the same entity+stage — suppressing the
	// success event here would break that. Retries of the SAME outcome
	// de-dup; transitions always emit.
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	ctx := ctxWithVC(t, emitterVC())
	em.Record(ctx, "capture", biz.ResultFailed)
	em.Record(ctx, "capture", biz.ResultFailed)  // retry: suppressed
	em.Record(ctx, "capture", biz.ResultSuccess) // transition: emits
	metrics, events := flushAndSnapshot(t, em, exp)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (failed once, success once)", len(events))
	}
	txn := metricsByName(metrics)["biz_txn_total"]
	var total int64
	for _, p := range txn {
		total += p.Value
	}
	if total != 2 {
		t.Fatalf("txn count %d, want 2", total)
	}
}

func TestOverflowDropsAreCounted(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp,
		WithClock(func() time.Time { return testClock }),
		WithBufferSize(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	ctx := ctxWithVC(t, emitterVC())
	// Unique entities so de-dup never intervenes; buffer of 2 must
	// overflow on the rest.
	for i := 0; i < 10; i++ {
		vc := emitterVC()
		vc.EntityID = "inv_" + string(rune('a'+i))
		em.Record(ctxWithVC(t, vc), "capture", biz.ResultFailed)
	}
	_ = ctx
	metrics, events := flushAndSnapshot(t, em, exp)
	drops := metricsByName(metrics)["biz_dropped_events_total"]
	var dropped int64
	for _, p := range drops {
		if p.Labels["reason"] == "overflow" {
			dropped += p.Value
		}
	}
	if int64(len(events))+dropped != 10 {
		t.Fatalf("events %d + overflow drops %d != 10", len(events), dropped)
	}
	if dropped == 0 {
		t.Fatal("expected overflow drops with a buffer of 2")
	}
}

func TestExportFailureCountsDrops(t *testing.T) {
	exp := &captureExporter{failAll: true}
	em := newTestEmitter(t, exp)
	em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed)
	em.Flush(context.Background())
	// Let the exporter recover, then flush again: the drop counter for
	// the failed export must reach the backend.
	exp.mu.Lock()
	exp.failAll = false
	exp.mu.Unlock()
	em.Record(ctxWithVC(t, emitterVC()), "settle", biz.ResultSuccess)
	metrics, _ := flushAndSnapshot(t, em, exp)
	drops := metricsByName(metrics)["biz_dropped_events_total"]
	var exportDrops int64
	for _, p := range drops {
		if p.Labels["reason"] == "export" {
			exportDrops += p.Value
		}
	}
	if exportDrops == 0 {
		t.Fatal("export failure left no visible drop count")
	}
}

func TestSetInFlightEmitsGauge(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.SetInFlight("invoice.pay", "capture", Age5mTo30m, biz.Money{Amount: 5568661, Currency: "USD", Exponent: 2})
	metrics, _ := flushAndSnapshot(t, em, exp)
	g := metricsByName(metrics)["biz_inflight_value"]
	if len(g) != 1 {
		t.Fatalf("gauge points: %v", metrics)
	}
	want := map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": Age5mTo30m, "currency": "USD"}
	for k, v := range want {
		if g[0].Labels[k] != v {
			t.Fatalf("gauge label %s = %q, want %q", k, g[0].Labels[k], v)
		}
	}
	if len(g[0].Labels) != len(want) {
		t.Fatalf("gauge label set %v, want exactly %v", g[0].Labels, want)
	}
	if g[0].Value != 5568661 {
		t.Fatalf("gauge value %d", g[0].Value)
	}
}

func TestSetInFlightRejectsUnknownBucket(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.SetInFlight("invoice.pay", "capture", "1m-5min", biz.Money{Amount: 1, Currency: "USD", Exponent: 2})
	metrics, _ := flushAndSnapshot(t, em, exp)
	if g := metricsByName(metrics)["biz_inflight_value"]; len(g) != 0 {
		t.Fatalf("typo bucket minted a series: %v", g)
	}
	drops := metricsByName(metrics)["biz_dropped_events_total"]
	if len(drops) != 1 || drops[0].Labels["reason"] != "invalid" {
		t.Fatalf("unknown bucket must count as invalid: %v", drops)
	}
}

func TestCloseFlushesAndShutsDown(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp, WithClock(func() time.Time { return testClock }))
	if err != nil {
		t.Fatal(err)
	}
	em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed)
	if err := em.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, e := exp.snapshot()
	if len(e) != 1 || len(m) == 0 {
		t.Fatalf("Close lost buffered signals: %d metrics, %d events", len(m), len(e))
	}
	if !exp.closed {
		t.Fatal("Close did not shut the exporter down")
	}
}

func BenchmarkRecord(b *testing.B) {
	exp := &captureExporter{}
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		b.Fatal(err)
	}
	em, err := New(&reg, exp, WithBufferSize(1<<20))
	if err != nil {
		b.Fatal(err)
	}
	defer em.Close(context.Background())
	ctx, err := biz.WithValueContext(context.Background(), emitterVC())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		em.Record(ctx, "capture", biz.ResultFailed)
	}
}
