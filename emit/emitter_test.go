package emit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

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
	if err := em.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
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
	// The key includes the result on purpose: the engine's realized leg
	// de-duplicates failures against later successes for the same
	// entity+stage — suppressing the success event here would break that.
	// Retries of the same outcome de-dup; transitions always emit.
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
	// Atomic drop: an overflowed observation contributes no metric
	// increments — sums and events cannot diverge through overflow.
	txn := metricsByName(metrics)["biz_txn_total"]
	var txnTotal int64
	for _, p := range txn {
		txnTotal += p.Value
	}
	if txnTotal != int64(len(events)) {
		t.Fatalf("txn metric total %d != exported events %d — overflow must drop atomically", txnTotal, len(events))
	}
}

func TestOverflowDropDoesNotPoisonRetry(t *testing.T) {
	// Regression: the de-dup set must not remember an observation that
	// overflow dropped, or the retry after the buffer drains is
	// suppressed forever and the event is lost.
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp,
		WithClock(func() time.Time { return testClock }),
		WithBufferSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })

	first := emitterVC()
	second := emitterVC()
	second.EntityID = "inv_002"
	em.Record(ctxWithVC(t, first), "capture", biz.ResultFailed)  // fills the buffer
	em.Record(ctxWithVC(t, second), "capture", biz.ResultFailed) // overflow-dropped
	if err := em.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	em.Record(ctxWithVC(t, second), "capture", biz.ResultFailed) // retry must emit
	_, events := flushAndSnapshot(t, em, exp)
	found := false
	for _, ev := range events {
		if ev.VC.EntityID == "inv_002" {
			found = true
		}
	}
	if !found {
		t.Fatal("retry after overflow was dedup-suppressed — the event is lost in-process")
	}
}

func TestExportFailureCountsDrops(t *testing.T) {
	exp := &captureExporter{failAll: true}
	em := newTestEmitter(t, exp)
	em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed)
	if err := em.Flush(context.Background()); err == nil {
		t.Fatal("failing export must surface through Flush")
	}
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
	em.SetInFlight("invoice.pay", "capture", Age5mTo30m, biz.Money{Amount: 5568661, Currency: "USD", Exponent: 2}, 42)
	metrics, _ := flushAndSnapshot(t, em, exp)
	byName := metricsByName(metrics)
	g := byName["biz_inflight_value"]
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
	// The companion count gauge (ADR-0012) rides the same labels.
	c := byName["biz_inflight_count"]
	if len(c) != 1 || c[0].Value != 42 {
		t.Fatalf("count gauge = %v, want one point value 42", c)
	}
	for k, v := range want {
		if c[0].Labels[k] != v {
			t.Fatalf("count label %s = %q, want %q", k, c[0].Labels[k], v)
		}
	}
}

func TestSetInFlightRejectsNegativeCount(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.SetInFlight("invoice.pay", "capture", Age5mTo30m, biz.Money{Amount: 1, Currency: "USD", Exponent: 2}, -1)
	metrics, _ := flushAndSnapshot(t, em, exp)
	if g := metricsByName(metrics)["biz_inflight_value"]; len(g) != 0 {
		t.Fatalf("a negative count must reject the whole sample: %v", g)
	}
	drops := metricsByName(metrics)["biz_dropped_events_total"]
	if len(drops) != 1 || drops[0].Labels["reason"] != "invalid" {
		t.Fatalf("negative count must count as invalid: %v", drops)
	}
}

func TestSetInFlightRejectsUnknownBucket(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.SetInFlight("invoice.pay", "capture", "1m-5min", biz.Money{Amount: 1, Currency: "USD", Exponent: 2}, 1)
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

func TestRecordOptionOverrides(t *testing.T) {
	webhookAt := time.Date(2026, 8, 27, 9, 15, 0, 0, time.UTC) // hours before receipt
	cases := []struct {
		name  string
		opts  []Option
		check func(t *testing.T, ev biz.Outcome)
	}{
		{"WithAt pins provider event time", []Option{WithAt(webhookAt)}, func(t *testing.T, ev biz.Outcome) {
			if !ev.At.Equal(webhookAt) {
				t.Fatalf("At = %v, want the provider timestamp %v — receipt-time stamping moves money across windows", ev.At, webhookAt)
			}
		}},
		{"WithErr carries the failure text", []Option{WithErr("capture timeout after 30s")}, func(t *testing.T, ev biz.Outcome) {
			if ev.Err != "capture timeout after 30s" {
				t.Fatalf("Err = %q", ev.Err)
			}
		}},
		{"WithSource tags the origin", []Option{WithSource("stripe:webhook")}, func(t *testing.T, ev biz.Outcome) {
			if ev.Source != "stripe:webhook" {
				t.Fatalf("Source = %q", ev.Source)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &captureExporter{}
			em := newTestEmitter(t, exp)
			em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed, c.opts...)
			_, events := flushAndSnapshot(t, em, exp)
			if len(events) != 1 {
				t.Fatalf("events = %d", len(events))
			}
			c.check(t, events[0])
		})
	}
}

func TestRecordLinksTraceID(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	ctx := trace.ContextWithSpanContext(ctxWithVC(t, emitterVC()), sc)
	em.Record(ctx, "capture", biz.ResultFailed)
	_, events := flushAndSnapshot(t, em, exp)
	if len(events) != 1 || events[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace link missing: %+v", events)
	}
	// And without a span: empty, valid.
	em2exp := &captureExporter{}
	em2 := newTestEmitter(t, em2exp)
	em2.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed)
	_, events2 := flushAndSnapshot(t, em2, em2exp)
	if len(events2) != 1 || events2[0].TraceID != "" {
		t.Fatalf("no-span record should have empty trace id: %+v", events2)
	}
}

func TestSetInFlightFencesFlowAndStage(t *testing.T) {
	cases := []struct {
		name                string
		flow, stage         string
		wantFlow, wantStage string
	}{
		{"registered", "invoice.pay", "capture", "invoice.pay", "capture"},
		{"unregistered flow", "totally.bogus", "capture", "unregistered", "unregistered"},
		{"unregistered stage", "invoice.pay", "refund", "invoice.pay", "unregistered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &captureExporter{}
			em := newTestEmitter(t, exp)
			em.SetInFlight(c.flow, c.stage, AgeLt1m, biz.Money{Amount: 1, Currency: "USD", Exponent: 2}, 1)
			metrics, _ := flushAndSnapshot(t, em, exp)
			g := metricsByName(metrics)["biz_inflight_value"]
			if len(g) != 1 {
				t.Fatalf("gauge points: %v", metrics)
			}
			if g[0].Labels["flow"] != c.wantFlow || g[0].Labels["stage"] != c.wantStage {
				t.Fatalf("labels %v, want flow=%s stage=%s — no caller string may mint a series", g[0].Labels, c.wantFlow, c.wantStage)
			}
		})
	}
}

func TestMetricBufferIsBounded(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp,
		WithClock(func() time.Time { return testClock }),
		WithBufferSize(1)) // metricsCap = 8
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	for i := 0; i < 100; i++ {
		em.SetInFlight("invoice.pay", "capture", AgeLt1m, biz.Money{Amount: int64(i + 1), Currency: "USD", Exponent: 2}, 1)
	}
	metrics, _ := flushAndSnapshot(t, em, exp)
	if len(metrics) > 8 {
		t.Fatalf("metric buffer exceeded its bound: %d points", len(metrics))
	}
}

func TestTwoGenSetEviction(t *testing.T) {
	s := newTwoGenSet(2)
	cases := []struct {
		name string
		key  string
		want bool // seen?
	}{
		{"a new", "a", false},
		{"a repeat", "a", true},
		{"b new", "b", false},
		{"c new rotates", "c", false},
		{"a survives one rotation via prev", "a", true},
		{"d new", "d", false},
		{"e rotates again", "e", false},
		{"b evicted after two rotations", "b", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.seen(c.key); got != c.want {
				t.Fatalf("seen(%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

func TestConcurrentRecordFlushClose(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp, WithFlushInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				vc := emitterVC()
				vc.EntityID = fmt.Sprintf("inv_%d_%d", g, i)
				ctx, err := biz.WithValueContext(context.Background(), vc)
				if err != nil {
					t.Error(err)
					return
				}
				em.Record(ctx, "capture", biz.ResultFailed)
				if i%50 == 0 {
					_ = em.Flush(context.Background())
				}
			}
		}(g)
	}
	wg.Wait()
	if err := em.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := em.Close(context.Background()); err != nil {
		t.Fatal("second Close must be idempotent and return the first result")
	}
	_, events := exp.snapshot()
	if len(events) != 8*200 {
		t.Fatalf("events = %d, want %d (nothing lost, nothing doubled)", len(events), 8*200)
	}
	if !exp.closed {
		t.Fatal("exporter not shut down")
	}
}

func TestBackgroundFlushDelivers(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp, WithFlushInterval(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, events := exp.snapshot(); len(events) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("background flusher never delivered")
}

func BenchmarkRecordAccept(b *testing.B) {
	exp := &captureExporter{}
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		b.Fatal(err)
	}
	em, err := New(&reg, exp, WithBufferSize(1<<22), WithFlushInterval(0))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = em.Close(context.Background()) }()
	ctxs := make([]context.Context, 4096)
	for i := range ctxs {
		vc := emitterVC()
		vc.EntityID = fmt.Sprintf("inv_%06d", i)
		ctxs[i], err = biz.WithValueContext(context.Background(), vc)
		if err != nil {
			b.Fatal(err)
		}
	}
	stages := [...]string{"auth", "capture", "settle"}
	results := [...]biz.Result{biz.ResultFailed, biz.ResultSuccess, biz.ResultDeferred, biz.ResultUnknown}
	// delivered totals what actually reached the sink, so the conservation
	// check below can prove every call took the accept path.
	delivered := 0
	drain := func() {
		_ = em.Flush(context.Background())
		exp.mu.Lock()
		delivered += len(exp.events)
		exp.events, exp.metrics = nil, nil
		exp.mu.Unlock()
		// Forget every key seen so far. The entity pool is far smaller than
		// twoGenSet's 1<<16 capacity, so without this the set never rotates,
		// every key stays remembered, and all but the first pass through the
		// pool is suppressed — which is what this benchmark used to price
		// while calling itself Accept.
		// clear() rather than a fresh set: reallocating ~1 MB every block
		// leaves its GC work inside the timed region, and this benchmark's
		// whole point is to price nothing but the accept path (ADR-0015
		// asks for the same). cur holds one block's 4096 keys against a
		// 65536 capacity, so it never rotates and prev stays nil.
		em.mu.Lock()
		clear(em.dedup.cur)
		em.mu.Unlock()
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// One block is one pass over the entity pool at a fixed stage and
		// result, so every key inside a block is distinct; the drain between
		// blocks makes the next block distinct from this one.
		em.Record(ctxs[i%len(ctxs)], stages[(i/len(ctxs))%len(stages)], results[(i/(len(ctxs)*len(stages)))%len(results)])
		if i%len(ctxs) == len(ctxs)-1 {
			b.StopTimer()
			drain()
			b.StartTimer()
		}
	}
	b.StopTimer()
	drain()

	// Conservation, borrowed from BenchmarkRecordParallel: if any call had
	// been suppressed or dropped this count would fall short, and the ns/op
	// would describe a path nobody asked about. A benchmark that cannot tell
	// which path it measured is how the published accept figure came to
	// describe de-dup hits.
	if delivered != b.N {
		b.Fatalf("delivered %d outcomes for %d Record calls — the benchmark stopped measuring the accept path", delivered, b.N)
	}
}

func BenchmarkRecordSuppressed(b *testing.B) {
	exp := &captureExporter{}
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		b.Fatal(err)
	}
	em, err := New(&reg, exp, WithFlushInterval(0))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = em.Close(context.Background()) }()
	ctx, err := biz.WithValueContext(context.Background(), emitterVC())
	if err != nil {
		b.Fatal(err)
	}
	em.Record(ctx, "capture", biz.ResultFailed) // prime the dedup
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		em.Record(ctx, "capture", biz.ResultFailed)
	}
}

func TestEstimatedOutcomeStaysOutOfValueMetric(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	vc := emitterVC()
	vc.Estimated = true
	em.Record(ctxWithVC(t, vc), "auth", biz.ResultAbandoned)
	metrics, events := flushAndSnapshot(t, em, exp)
	byName := metricsByName(metrics)
	if len(byName["biz_value_total"]) != 0 {
		t.Fatalf("estimated value leaked into biz_value_total: %v", byName["biz_value_total"])
	}
	if len(byName["biz_txn_total"]) != 1 {
		t.Fatalf("estimated outcome should still count: %v", byName["biz_txn_total"])
	}
	if len(events) != 1 || !events[0].VC.Estimated {
		t.Fatalf("estimated outcome must still ride the event: %+v", events)
	}
}

func TestRecordProviderCallEmitsCounter(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)
	em.RecordProviderCall("stripe", "capture", ProviderCallFailed)
	metrics, _ := flushAndSnapshot(t, em, exp)
	pts := metricsByName(metrics)["biz_provider_calls_total"]
	if len(pts) != 1 {
		t.Fatalf("provider-call points: %v", metrics)
	}
	want := map[string]string{"provider": "stripe", "op": "capture", "outcome": ProviderCallFailed}
	for k, v := range want {
		if pts[0].Labels[k] != v {
			t.Fatalf("label %s = %q, want %q", k, pts[0].Labels[k], v)
		}
	}
	if len(pts[0].Labels) != len(want) {
		t.Fatalf("label set %v, want exactly the ADR-0004 set %v", pts[0].Labels, want)
	}
	if pts[0].Value != 1 {
		t.Fatalf("counter delta %d, want 1", pts[0].Value)
	}
	if !pts[0].At.Equal(testClock) {
		t.Fatalf("At = %v, want the emitter clock %v", pts[0].At, testClock)
	}
}

func TestRecordProviderCallRejectsUnboundedLabels(t *testing.T) {
	cases := []struct {
		name         string
		provider, op string
		outcome      string
	}{
		{"empty provider", "", "capture", ProviderCallSuccess},
		{"empty op", "stripe", "", ProviderCallSuccess},
		{"blank provider", "   ", "capture", ProviderCallSuccess},
		{"outcome outside the enum", "stripe", "capture", "deferred"},
		{"empty outcome", "stripe", "capture", ""},
		{"over-long op", "stripe", strings.Repeat("x", 65), ProviderCallSuccess},
		{"control character", "stripe", "cap\nture", ProviderCallSuccess},
		// Invalid UTF-8 must never reach a label: the OTLP exporter
		// marshals label values as protobuf strings, which fails the
		// WHOLE batch on a bad byte, and Flush drops a failed metric
		// batch entire — one bad op would take other families with it.
		{"invalid utf-8", "stripe", "cap\xffture", ProviderCallSuccess},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &captureExporter{}
			em := newTestEmitter(t, exp)
			em.RecordProviderCall(c.provider, c.op, c.outcome)
			metrics, _ := flushAndSnapshot(t, em, exp)
			if pts := metricsByName(metrics)["biz_provider_calls_total"]; len(pts) != 0 {
				t.Fatalf("a rejected call minted a series: %v", pts)
			}
			drops := metricsByName(metrics)["biz_dropped_events_total"]
			if len(drops) != 1 || drops[0].Labels["reason"] != "invalid" {
				t.Fatalf("rejection must be loud on biz_dropped_events_total{reason=invalid}, got %v", drops)
			}
		})
	}
}

func TestRecordProviderCallCapsDistinctPairs(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp,
		WithClock(func() time.Time { return testClock }),
		WithProviderPairCap(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })

	// Two distinct pairs fit under the cap and keep their own labels.
	em.RecordProviderCall("stripe", "capture", ProviderCallSuccess)
	em.RecordProviderCall("stripe", "refund", ProviderCallSuccess)
	// A third distinct pair is past the cap: it still counts, but it may not
	// mint a third series — ADR-0004 cardinality is the library's guarantee,
	// not the caller's discipline.
	em.RecordProviderCall("stripe", "unbounded-per-request-op", ProviderCallFailed)

	metrics, _ := flushAndSnapshot(t, em, exp)
	pts := metricsByName(metrics)["biz_provider_calls_total"]
	if len(pts) != 3 {
		t.Fatalf("every call must still be counted: %v", pts)
	}
	seen := map[string]int{}
	for _, p := range pts {
		seen[p.Labels["provider"]+"/"+p.Labels["op"]]++
	}
	if seen["stripe/capture"] != 1 || seen["stripe/refund"] != 1 {
		t.Fatalf("pairs under the cap must keep their labels: %v", seen)
	}
	if seen[ProviderOther+"/"+ProviderOther] != 1 {
		t.Fatalf("a pair past the cap must collapse to the %q sentinel, got %v", ProviderOther, seen)
	}
	if seen["stripe/unbounded-per-request-op"] != 0 {
		t.Fatalf("a pair past the cap minted a series: %v", seen)
	}

	// The sentinel is itself a stable series, and an already-admitted pair
	// still passes after the cap is reached.
	em.RecordProviderCall("stripe", "another-new-op", ProviderCallFailed)
	em.RecordProviderCall("stripe", "capture", ProviderCallSuccess)
	metrics2, _ := flushAndSnapshot(t, em, exp)
	after := map[string]int{}
	for _, p := range metricsByName(metrics2)["biz_provider_calls_total"] {
		after[p.Labels["provider"]+"/"+p.Labels["op"]]++
	}
	if after[ProviderOther+"/"+ProviderOther] != 2 || after["stripe/capture"] != 2 {
		t.Fatalf("sentinel must be stable and admitted pairs must survive the cap: %v", after)
	}
}

func TestRecordProviderCallIsOnTheEmitterInterface(t *testing.T) {
	// The engine reads biz_provider_calls_total{outcome=failed}; the interface
	// is what makes that readable counter writable by any Emitter, not just Std.
	var em Emitter = newTestEmitter(t, &captureExporter{})
	em.RecordProviderCall("stripe", "capture", ProviderCallFailed)
}

// lockProbeHandler asserts, from inside the log handler, that the emitter's
// mutex is NOT held while it runs. A handler write under s.mu blocks Record
// on the request path — the one thing Record promises not to do.
type lockProbeHandler struct {
	slog.Handler
	em     *Std
	t      *testing.T
	probed atomic.Bool
}

func (h *lockProbeHandler) Handle(ctx context.Context, r slog.Record) error {
	h.probed.Store(true)
	if !h.em.mu.TryLock() {
		h.t.Errorf("emit: %q logged while holding the emitter mutex — that blocks Record on the request path", r.Message)
	} else {
		h.em.mu.Unlock()
	}
	return nil
}

func TestRecordProviderCallLogsWithTheLockReleased(t *testing.T) {
	// The probe needs the *Std it will interrogate, and the emitter needs
	// the probe as its logger, so the probe is built first and its target
	// filled in after New. WithFlushInterval(0) keeps that safe: no
	// background flusher exists to read s.logger, so nothing can log
	// before this goroutine sets em.
	probe := &lockProbeHandler{Handler: slog.NewTextHandler(io.Discard, nil), t: t}
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp,
		WithClock(func() time.Time { return testClock }),
		WithFlushInterval(0),
		WithLogger(slog.New(probe)),
		WithProviderPairCap(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	probe.em = em

	em.RecordProviderCall("stripe", "capture", ProviderCallSuccess) // admitted, no log
	em.RecordProviderCall("stripe", "past-the-cap", ProviderCallFailed)
	if !probe.probed.Load() {
		t.Fatal("the over-cap branch never logged, so the lock was never probed")
	}

	// The rejection path logs too, and must also run unlocked.
	em.RecordProviderCall("stripe", "", ProviderCallSuccess)
}

// TestDroppedEventsCountSurvivesAMetricExportFailure covers both sides of the
// one exception in Flush's failed-metric-batch policy.
//
// A failed metric batch is logged and dropped without counting — re-queuing
// deltas risks double-count on a partial write. The exception is
// biz_dropped_events_total: those points are the record of the library's own
// damage and never left the process, so they are re-credited rather than
// lost. Neither side of that had a test: the only failure-injection test in
// the package fails a batch that carries no drop point at all.
func TestDroppedEventsCountSurvivesAMetricExportFailure(t *testing.T) {
	exp := &captureExporter{}
	em := newTestEmitter(t, exp)

	// Mint a drop the emitter itself owns, so dropCounts is non-empty and
	// the next flush carries a biz_dropped_events_total point.
	em.SetInFlight("invoice.pay", "capture", "not-a-bucket", biz.Money{Amount: 1, Currency: "USD", Exponent: 2}, 1)
	// And an ordinary metric alongside it, to pin the other half of the
	// policy: this one must NOT come back.
	em.Record(ctxWithVC(t, emitterVC()), "capture", biz.ResultFailed)

	exp.mu.Lock()
	exp.failAll = true
	exp.mu.Unlock()
	if err := em.Flush(context.Background()); err == nil {
		t.Fatal("failing export must surface through Flush")
	}

	exp.mu.Lock()
	exp.failAll = false
	exp.mu.Unlock()
	metrics, _ := flushAndSnapshot(t, em, exp)
	byName := metricsByName(metrics)

	// The WHOLE drop-point set, not just the invalid sum. Asserting one
	// reason cannot see the mutation that matters most: drop the branch's
	// family guard and every failed point's value folds into dropCounts,
	// minting biz_dropped_events_total{reason=""} out of a transaction
	// count and a money amount — the counter corruption this branch's
	// comment says re-crediting avoids.
	got := map[string]int64{}
	for _, p := range byName["biz_dropped_events_total"] {
		got[p.Labels["reason"]] += p.Value
	}
	want := map[string]int64{"invalid": 1, "export": 1}
	if !maps.Equal(got, want) {
		t.Errorf("biz_dropped_events_total after a failed export = %v, want %v — "+
			"a backend outage must not destroy the record of the library's own damage, "+
			"and nothing but that record may be re-credited", got, want)
	}
	// The transaction families are NOT re-credited: re-queuing them past a
	// partial write is how a counter double-counts.
	if pts := byName["biz_txn_total"]; len(pts) != 0 {
		t.Errorf("biz_txn_total came back after a failed export (%d points) — only the drop counters are re-credited", len(pts))
	}
	if pts := byName["biz_value_total"]; len(pts) != 0 {
		t.Errorf("biz_value_total came back after a failed export (%d points)", len(pts))
	}
}
