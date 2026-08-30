package otlp

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// capturingLogExporter keeps every record it is handed, so a test can assert
// on what was actually delivered rather than what was accepted.
type capturingLogExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
	fail error
}

func (c *capturingLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	if c.fail != nil {
		return c.fail
	}
	c.mu.Lock()
	c.recs = append(c.recs, recs...)
	c.mu.Unlock()
	return nil
}
func (c *capturingLogExporter) Shutdown(context.Context) error   { return nil }
func (c *capturingLogExporter) ForceFlush(context.Context) error { return nil }

func (c *capturingLogExporter) all() []sdklog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sdklog.Record(nil), c.recs...)
}

func outcomes(n int) []biz.Outcome {
	out := make([]biz.Outcome, 0, n)
	for range n {
		out = append(out, biz.Outcome{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed})
	}
	return out
}

// TestEventExportFailureSurfaces pins that a terminal export failure reaches
// emit, which is what lets it count the drop on biz_dropped_events_total
// rather than believing the batch landed.
func TestEventExportFailureSurfaces(t *testing.T) {
	exp := &capturingLogExporter{fail: errors.New("collector unreachable")}
	sink := newProviderSink(exp, defaultResource())
	if err := sink.emit(context.Background(), outcomes(3)); err == nil {
		t.Fatal("want an error, got nil — a failed event export must not report success")
	}
}

// TestTraceLinking covers providerSink.emit's branching: an outcome links to
// its trace when it carries a usable id, and emits unlinked rather than being
// dropped when it does not.
func TestTraceLinking(t *testing.T) {
	cases := []struct {
		name    string
		traceID string
		want    bool
	}{
		{"no trace id emits unlinked", "", false},
		{"valid trace id links", "4bf92f3577b34da6a3ce929d0e0e4736", true},
		{"malformed trace id falls through unlinked", "not-hex", false},
		{"wrong-length trace id falls through unlinked", "4bf92f3577b34da6", false},
		{"all-zero trace id is invalid and does not link", "00000000000000000000000000000000", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &capturingLogExporter{}
			sink := newProviderSink(exp, defaultResource())
			o := biz.Outcome{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed, TraceID: c.traceID}
			if err := sink.emit(context.Background(), []biz.Outcome{o}); err != nil {
				t.Fatalf("emit: %v", err)
			}
			recs := exp.all()
			if len(recs) != 1 {
				t.Fatalf("delivered %d records, want 1", len(recs))
			}
			gotLinked := recs[0].TraceID().IsValid()
			if gotLinked != c.want {
				t.Errorf("trace linked = %v, want %v (trace id %q)", gotLinked, c.want, c.traceID)
			}
			if c.want {
				if got := recs[0].TraceID().String(); got != c.traceID {
					t.Errorf("linked trace id = %s, want %s", got, c.traceID)
				}
				// Backends that gate log-to-trace correlation on the sampled
				// flag drop the link when it is unset.
				if !recs[0].TraceFlags().IsSampled() {
					t.Error("record is not marked sampled — backends that gate correlation on the flag will not link it")
				}
			}
		})
	}
}

// TestRecordCarriesEveryBizField pins the delivered record's attributes, not
// just the builder's output: an attribute limit or a processor change that
// silently trimmed biz.* fields would pass a builder-only test.
func TestRecordCarriesEveryBizField(t *testing.T) {
	exp := &capturingLogExporter{}
	sink := newProviderSink(exp, defaultResource())
	o := biz.Outcome{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed, Source: "stripe:webhook"}
	if err := sink.emit(context.Background(), []biz.Outcome{o}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	recs := exp.all()
	if len(recs) != 1 {
		t.Fatalf("delivered %d records, want 1", len(recs))
	}
	got := map[string]attribute.Value{}
	recs[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		got[string(kv.Key)] = kv.Value
		return true
	})
	for _, key := range []string{
		"biz.flow", "biz.stage", "biz.outcome", "biz.entity.id", "biz.customer.id",
		"biz.amount_minor", "biz.currency", "biz.exponent", "biz.value.kind",
		"biz.amount.est", "biz.segment", "source",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("delivered record is missing %s", key)
		}
	}
	if v := got["biz.amount_minor"]; v.AsInt64() != 14900 {
		t.Errorf("biz.amount_minor = %v, want 14900", v)
	}
	if recs[0].EventName() != eventName {
		t.Errorf("event name = %q, want %q", recs[0].EventName(), eventName)
	}
}

// TestUnknownMetricFamilyIsRejected matches the guarantee the sibling
// exporters enforce. Shipping an unrecognised family as a monotonic counter
// is worse than refusing it: a mistyped or newly added level family would be
// summed by the backend.
func TestUnknownMetricFamilyIsRejected(t *testing.T) {
	cases := []struct {
		name   string
		family string
	}{
		{"typo in a known family", "biz_txn_totl"},
		{"not a biz family at all", "http_requests_total"},
		{"empty name", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &fakeMetric{}
			e := newWith(m, &fakeLog{})
			batch := []emit.MetricPoint{{Name: c.family, Labels: map[string]string{"flow": "f"}, Value: 1, At: at}}
			err := e.ExportMetrics(context.Background(), batch)
			if err == nil {
				t.Fatal("want an error, got nil — an unrecognised family must not ship under a guessed kind")
			}
			if !strings.Contains(err.Error(), "unknown metric family") {
				t.Errorf("error = %q, want it to name the unknown family", err)
			}
			if m.got != nil {
				t.Error("a rejected batch reached the backend")
			}
		})
	}
}

// TestEveryPointCarriesWriterIdentity pins that replicas cannot share a gauge
// series. The ADR-0004 label sets carry no writer identity, and a gauge is a
// level, so without a per-process resource attribute the backend keeps one
// replica's in-flight value and the deferred leg under-reports by roughly the
// replica count.
func TestEveryPointCarriesWriterIdentity(t *testing.T) {
	m := &fakeMetric{}
	e := newWith(m, &fakeLog{})
	batch := []emit.MetricPoint{{
		Name:   "biz_inflight_value",
		Labels: map[string]string{"flow": "f", "stage": "s", "age_bucket": "5m-30m", "currency": "USD"},
		Value:  500, At: at,
	}}
	if err := e.ExportMetrics(context.Background(), batch); err != nil {
		t.Fatalf("export: %v", err)
	}
	if m.got.Resource == nil {
		t.Fatal("exported points carry no resource — replicas would share one gauge series")
	}
	attrs := map[string]string{}
	for _, kv := range m.got.Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["service.name"] == "" {
		t.Error("resource carries no service.name")
	}
	inst := attrs["service.instance.id"]
	if inst == "" {
		t.Fatal("resource carries no service.instance.id — nothing distinguishes one replica from another")
	}
	if !strings.Contains(inst, strconv.Itoa(os.Getpid())) {
		t.Errorf("service.instance.id = %q, want it to distinguish this process (pid %d)", inst, os.Getpid())
	}
}

// TestBothSignalsCarryTheSameWriterIdentity pins that metrics and events agree
// about who wrote them. The log pipeline defaults to the SDK's own resource
// when it is not given one, which would leave events under
// "unknown_service:<binary>" with no instance id while metrics carried the
// real identity — and a backend correlating the two legs by service.name
// could not.
func TestBothSignalsCarryTheSameWriterIdentity(t *testing.T) {
	exp := &capturingLogExporter{}
	res := defaultResource()
	sink := newProviderSink(exp, res)
	if err := sink.emit(context.Background(), outcomes(1)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	recs := exp.all()
	if len(recs) != 1 {
		t.Fatalf("delivered %d records, want 1", len(recs))
	}
	eventAttrs := attrSet(recs[0].Resource().Attributes())

	// Both legs are built from one resource value, so comparing them proves
	// it reaches each leg but says nothing about what it holds. The literal
	// below is what pins the content; the comparison is what pins the
	// plumbing. Neither substitutes for the other.
	rm, err := buildResourceMetrics([]emit.MetricPoint{{
		Name:   "biz_txn_total",
		Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
		Value:  1, At: at,
	}}, res)
	if err != nil {
		t.Fatalf("build metrics: %v", err)
	}
	metricAttrs := attrSet(rm.Resource.Attributes())

	if got := eventAttrs["service.name"]; got != "shortfall" {
		t.Errorf("service.name = %q, want shortfall", got)
	}
	for _, key := range []string{"service.name", "service.instance.id"} {
		if eventAttrs[key] == "" {
			t.Errorf("event leg carries no %s", key)
		}
		if eventAttrs[key] != metricAttrs[key] {
			t.Errorf("%s differs between legs: events %q, metrics %q — a backend correlating them cannot", key, eventAttrs[key], metricAttrs[key])
		}
	}
	if !strings.Contains(eventAttrs["service.instance.id"], strconv.Itoa(os.Getpid())) {
		t.Errorf("service.instance.id = %q, want it to name this process (pid %d)", eventAttrs["service.instance.id"], os.Getpid())
	}
}

func attrSet(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// boundedExporter records how many records arrive between one ForceFlush and
// the next: no more than the queue's capacity may be in flight before it is
// drained. That is a fact about what the code does rather than about which
// goroutine wins, so it holds identically under the race detector — where an
// assertion about lost records does not, since loss needs the producer to
// outrun the consumer.
type boundedExporter struct {
	mu        sync.Mutex
	sinceLast int
	maxRun    int
	total     int
}

func (b *boundedExporter) Export(_ context.Context, recs []sdklog.Record) error {
	b.mu.Lock()
	b.sinceLast += len(recs)
	b.total += len(recs)
	if b.sinceLast > b.maxRun {
		b.maxRun = b.sinceLast
	}
	b.mu.Unlock()
	return nil
}
func (b *boundedExporter) Shutdown(context.Context) error { return nil }
func (b *boundedExporter) ForceFlush(context.Context) error {
	b.mu.Lock()
	b.sinceLast = 0
	b.mu.Unlock()
	return nil
}
func (b *boundedExporter) peak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxRun
}
func (b *boundedExporter) delivered() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// TestEmitNeverExceedsQueueBetweenFlushes pins the chunking guarantee without
// depending on relative speed: whatever the batch size, the provider is never
// handed more than one queue's worth of records before a flush drains it. An
// unchunked emit hands it the whole batch, which is what overflows the queue
// and destroys outcome events. This assertion holds identically with and
// without the race detector.
func TestEmitNeverExceedsQueueBetweenFlushes(t *testing.T) {
	cases := []struct {
		name  string
		count int
	}{
		{"a partial chunk", 10},
		{"exactly one chunk", eventQueueSize},
		{"one past a chunk", eventQueueSize + 1},
		{"several chunks", eventQueueSize * 3},
		{"emit's full default event buffer", 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := &boundedExporter{}
			sink := newProviderSink(exp, defaultResource())
			if err := sink.emit(context.Background(), outcomes(c.count)); err != nil {
				t.Fatalf("emit: %v", err)
			}
			if err := sink.Shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			if got := exp.peak(); got > eventQueueSize {
				t.Errorf("%d records reached the provider between flushes, queue holds %d — the excess is silently overwritten", got, eventQueueSize)
			}
			if got := exp.delivered(); got != c.count {
				t.Errorf("delivered %d of %d outcomes", got, c.count)
			}
		})
	}
}

// TestConcurrentEmitNeverExceedsQueue is the per-sink half of the same
// guarantee: the queue is shared across calls, so overlapping ExportEvents
// calls must not put more than its capacity in flight between drains. Why
// overlap is reachable at all is on providerSink.mu.
func TestConcurrentEmitNeverExceedsQueue(t *testing.T) {
	const callers, per = 4, eventQueueSize
	exp := &boundedExporter{}
	sink := newProviderSink(exp, defaultResource())

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sink.emit(context.Background(), outcomes(per)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("emit: %v", err)
	}
	if err := sink.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := exp.peak(); got > eventQueueSize {
		t.Errorf("%d records reached the provider between flushes across %d concurrent callers, queue holds %d — the excess is silently overwritten", got, callers, eventQueueSize)
	}
	if got, want := exp.delivered(), callers*per; got != want {
		t.Errorf("delivered %d of %d outcomes", got, want)
	}
}

// TestPostShutdownExportsAreLoud pins the answer this adapter gives to a
// question emit.Exporter's own contract leaves open: an Export after
// Shutdown must fail, not report success having delivered nothing.
//
// It used to differ per leg. The metric leg errored (otlpmetrichttp swaps in
// a shutdownClient), while the event leg returned nil: sdklog's stopped
// provider discards the record in OnEmit and answers ForceFlush with nil, so
// providerSink.emit reported success for a batch nobody received — the
// silent drop ADR-0002 forbids. Same object, same call, opposite honesty.
func TestPostShutdownExportsAreLoud(t *testing.T) {
	logExp := &capturingLogExporter{}
	metricExp := &fakeMetric{}
	e := newWith(metricExp, newProviderSink(logExp, defaultResource()))

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	evErr := e.ExportEvents(context.Background(), outcomes(3))
	if evErr == nil {
		t.Error("ExportEvents after Shutdown returned nil — a batch nobody received must not report success")
	}
	mErr := e.ExportMetrics(context.Background(), []emit.MetricPoint{{
		Name:   "biz_txn_total",
		Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "segment": "smb"},
		Value:  1, At: at,
	}})
	if mErr == nil {
		t.Error("ExportMetrics after Shutdown returned nil")
	}
	// Both legs of one exporter must answer the same question the same way.
	if (evErr == nil) != (mErr == nil) {
		t.Errorf("legs disagree about post-Shutdown exports: events=%v metrics=%v", evErr, mErr)
	}
	if got := len(logExp.all()); got != 0 {
		t.Errorf("delivered %d records after Shutdown, want 0", got)
	}
}

// An empty batch drops nothing, so there is nothing to be loud about — and
// testkit/conformance's empty-batch invariant depends on this staying a
// no-op.
func TestPostShutdownEmptyBatchStaysANoop(t *testing.T) {
	e := newWith(&fakeMetric{}, newProviderSink(&capturingLogExporter{}, defaultResource()))
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := e.ExportEvents(context.Background(), nil); err != nil {
		t.Errorf("empty ExportEvents after Shutdown: %v", err)
	}
	if err := e.ExportMetrics(context.Background(), nil); err != nil {
		t.Errorf("empty ExportMetrics after Shutdown: %v", err)
	}
}
