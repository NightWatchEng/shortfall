package otlp

import (
	"context"
	"errors"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"

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
	if !errors.Is(evErr, ErrShutdown) {
		t.Errorf("ExportEvents after Shutdown = %v, want ErrShutdown — a batch nobody received must not report success", evErr)
	}
	mErr := e.ExportMetrics(context.Background(), []emit.MetricPoint{{
		Name:   "biz_txn_total",
		Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "segment": "smb"},
		Value:  1, At: at,
	}})
	if !errors.Is(mErr, ErrShutdown) {
		t.Errorf("ExportMetrics after Shutdown = %v, want ErrShutdown", mErr)
	}
	if got := len(logExp.all()); got != 0 {
		t.Errorf("delivered %d records after Shutdown, want 0", got)
	}
}

// An empty batch drops nothing, so there is nothing to be loud about. Note
// testkit/conformance's empty-batch invariant does NOT cover this: it runs
// both empty exports on a fresh exporter before Shutdown, so it is
// insensitive to where the guard sits. This test is the only thing pinning
// that placement.
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

// TestBothLegsResolveTheSameResource pins the whole resolved resource, not
// just the writer identity. sdklog.WithResource merges resource.Environment()
// into whatever it is given; the metric leg hand-builds its ResourceMetrics
// and never touches the metric SDK, so nothing merged the environment in
// there. A user who set OTEL_RESOURCE_ATTRIBUTES the standard way got those
// attributes on events and not on metrics.
//
// The service.name assertion under a hostile OTEL_SERVICE_NAME is not
// decoration: resource.Merge is last-wins, so merging in the wrong argument
// order produces an identical attribute set except that the environment
// captures service.name — and returns no error while doing it. Only this
// assertion separates the two orders.
func TestBothLegsResolveTheSameResource(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,k8s.pod.name=pod-7")
	t.Setenv("OTEL_SERVICE_NAME", "hijack")

	res := resolveResource(defaultResource())

	exp := &capturingLogExporter{}
	sink := newProviderSink(exp, res)
	if err := sink.emit(context.Background(), outcomes(1)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	recs := exp.all()
	if len(recs) != 1 {
		t.Fatalf("delivered %d records, want 1", len(recs))
	}
	eventAttrs := attrSet(recs[0].Resource().Attributes())

	// Through Exporter.ExportMetrics, not buildResourceMetrics directly: what
	// the metric leg SHIPS is the thing under test, and a helper-only
	// assertion cannot see a regression one line downstream of it.
	pusher := &fakeMetric{}
	shipping := newWith(pusher, newProviderSink(&capturingLogExporter{}, res))
	if err := shipping.ExportMetrics(context.Background(), []emit.MetricPoint{{
		Name:   "biz_txn_total",
		Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
		Value:  1, At: at,
	}}); err != nil {
		t.Fatalf("export metrics: %v", err)
	}
	if pusher.got == nil {
		t.Fatal("metric leg shipped nothing")
	}
	rm := pusher.got
	metricAttrs := attrSet(rm.Resource.Attributes())

	// Full-set equality, not a key list: a key list cannot see an asymmetry
	// in a key nobody thought to name.
	if !maps.Equal(eventAttrs, metricAttrs) {
		t.Errorf("legs resolve different resources:\n events  %v\n metrics %v", eventAttrs, metricAttrs)
	}
	for key, want := range map[string]string{
		"deployment.environment": "prod",
		"k8s.pod.name":           "pod-7",
	} {
		if eventAttrs[key] != want {
			t.Errorf("event leg %s = %q, want %q", key, eventAttrs[key], want)
		}
		if metricAttrs[key] != want {
			t.Errorf("metric leg %s = %q, want %q — OTEL_RESOURCE_ATTRIBUTES must reach both signals", key, metricAttrs[key], want)
		}
	}
	// The explicit resource must still win: OTEL_SERVICE_NAME cannot rename
	// the writer out from under the instance id.
	for name, got := range map[string]string{"events": eventAttrs["service.name"], "metrics": metricAttrs["service.name"]} {
		if got != "shortfall" {
			t.Errorf("%s service.name = %q, want shortfall — the explicit resource must win the merge", name, got)
		}
	}
	// A schema URL of "" would mean resource.Default() had been merged in,
	// whose semconv version differs from ours and conflicts.
	if got := rm.Resource.SchemaURL(); got != semconv.SchemaURL {
		t.Errorf("metric resource schema URL = %q, want %q", got, semconv.SchemaURL)
	}

	// Everything above tests the helper. These pin that the constructors
	// actually route through it — without them the merge could be deleted
	// from New and newWith and this test would still pass.
	built := newWith(&fakeMetric{}, newProviderSink(&capturingLogExporter{}, res))
	if got := attrSet(built.resource.Attributes())["deployment.environment"]; got != "prod" {
		t.Errorf("newWith resource deployment.environment = %q, want prod — the constructor did not resolve the resource", got)
	}
	e, err := New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	if got := attrSet(e.resource.Attributes())["deployment.environment"]; got != "prod" {
		t.Errorf("New resource deployment.environment = %q, want prod — the constructor did not resolve the resource", got)
	}
	if got := attrSet(e.resource.Attributes())["service.name"]; got != "shortfall" {
		t.Errorf("New resource service.name = %q, want shortfall", got)
	}
	// Both legs, not just the field the metric leg reads: New builds the log
	// sink separately, so handing it a different resource would leave events
	// carrying one identity and metrics another.
	sink, ok := e.logs.(*providerSink)
	if !ok {
		t.Fatalf("New built %T, want *providerSink", e.logs)
	}
	if !maps.Equal(attrSet(sink.res.Attributes()), attrSet(e.resource.Attributes())) {
		t.Errorf("New gave the legs different resources:\n events  %v\n metrics %v",
			attrSet(sink.res.Attributes()), attrSet(e.resource.Attributes()))
	}
}

// TestWithResourceMergesRatherThanOverrides covers the exported option whose
// contract this change rewrote. defaultResource() happens to carry both
// identity keys, so testing only through it cannot see what happens to a
// caller-supplied resource that omits one.
func TestWithResourceMergesRatherThanOverrides(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod")
	t.Setenv("OTEL_SERVICE_NAME", "from-env")

	named := attrSet(resolveResource(resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName("custom"), semconv.ServiceInstanceID("replica-1"))).Attributes())
	if named["service.name"] != "custom" {
		t.Errorf("service.name = %q, want custom — a resource that names itself must win", named["service.name"])
	}
	if named["service.instance.id"] != "replica-1" {
		t.Errorf("service.instance.id = %q, want replica-1", named["service.instance.id"])
	}
	if named["deployment.environment"] != "prod" {
		t.Errorf("deployment.environment = %q, want prod — the environment must still be merged in", named["deployment.environment"])
	}

	// A caller-supplied resource that does NOT name the service leaves that
	// key to the environment rather than blanking it.
	unnamed := attrSet(resolveResource(resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceInstanceID("replica-2"))).Attributes())
	if unnamed["service.name"] != "from-env" {
		t.Errorf("service.name = %q, want from-env — an unnamed resource must not shadow OTEL_SERVICE_NAME", unnamed["service.name"])
	}
	if unnamed["service.instance.id"] != "replica-2" {
		t.Errorf("service.instance.id = %q, want replica-2", unnamed["service.instance.id"])
	}
}

// TestShutdownRacingAnExportIsNeverSilent is the test the single-threaded
// post-Shutdown case could not be: it drives the window between the
// Exporter-level check and the sink's lock.
//
// emit.Std.Flush releases its own lock before calling the exporter, so a
// caller-driven Flush can still be inside ExportEvents when Close reaches
// Shutdown — the sink's own doc names that concurrency. If the sink is
// stopped while such a call is in flight, sdklog discards the records and
// answers ForceFlush with nil, and the caller is told a batch of outcome
// events landed when none did. Deciding under mu is what orders the two.
func TestShutdownRacingAnExportIsNeverSilent(t *testing.T) {
	const iters = 2000
	const batch = 3
	silent := 0
	for range iters {
		logExp := &capturingLogExporter{}
		e := newWith(&fakeMetric{}, newProviderSink(logExp, defaultResource()))

		var wg sync.WaitGroup
		var exportErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			exportErr = e.ExportEvents(context.Background(), outcomes(batch))
		}()
		go func() {
			defer wg.Done()
			_ = e.Shutdown(context.Background())
		}()
		wg.Wait()

		// Reported success while delivering fewer than it was handed. Partial
		// is the commoner mode — a provider stopped mid-batch — and a check
		// for zero delivered would miss it entirely.
		if exportErr == nil && len(logExp.all()) != batch {
			silent++
		}
	}
	if silent != 0 {
		t.Errorf("%d of %d races reported success having delivered less than the batch — that is the silent drop ADR-0002 forbids", silent, iters)
	}
}

// slowLogExporter holds the sink's slot for a fixed time, standing in for a
// real OTLP HTTP export with its own timeout and retries.
type slowLogExporter struct{ d time.Duration }

func (s *slowLogExporter) Export(context.Context, []sdklog.Record) error {
	time.Sleep(s.d)
	return nil
}
func (s *slowLogExporter) Shutdown(context.Context) error   { return nil }
func (s *slowLogExporter) ForceFlush(context.Context) error { return nil }

// TestShutdownHonorsItsDeadline pins that waiting for an in-flight emit does
// not mean waiting without bound. Shutdown must wait — that is what keeps a
// racing export from being cut short silently — but sync.Mutex.Lock cannot
// be cancelled, so waiting on one would pin a SIGTERM handler behind an OTLP
// export's own timeout and retries, well past any shutdown budget. The sink
// serialises on a channel for exactly this reason.
func TestShutdownHonorsItsDeadline(t *testing.T) {
	sink := newProviderSink(&slowLogExporter{d: 300 * time.Millisecond}, defaultResource())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sink.emit(context.Background(), outcomes(3))
	}()
	time.Sleep(20 * time.Millisecond) // let emit take the slot

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := sink.Shutdown(ctx)
	elapsed := time.Since(start)
	<-done

	if elapsed > 150*time.Millisecond {
		t.Errorf("Shutdown blocked %v on a 10ms deadline — a bounded shutdown budget cannot be honoured", elapsed)
	}
	if err == nil {
		t.Error("Shutdown that abandoned its wait returned nil — the caller must learn the drain did not finish")
	}
}
