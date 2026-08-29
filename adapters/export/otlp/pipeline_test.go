package otlp

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	// delay simulates a collector that cannot keep up, which is what makes an
	// overflow reachable at all: against an instant exporter the processor
	// drains as fast as a caller fills it, so no record is overwritten and the
	// fixture proves nothing about the queue.
	delay time.Duration
}

func (c *capturingLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
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

// TestNoOutcomeIsLostToQueueOverflow pins that a batch larger than the log
// SDK's queue loses nothing. The queue overwrites its oldest record on
// overflow and surfaces it through no channel the caller can reach, and
// emit's event buffer is several times the queue's size, so an unchunked
// batch would destroy outcome events while every layer reported success.
// Only the largest case below can catch that: the processor drains 512
// records per export, so a batch just over the queue size never fills it.
// The smaller cases pin the chunk arithmetic instead.
func TestNoOutcomeIsLostToQueueOverflow(t *testing.T) {
	cases := []struct {
		name  string
		count int
	}{
		{"chunk arithmetic: a partial chunk", 10},
		{"chunk boundary: exactly one chunk", eventQueueSize},
		{"chunk boundary: one past a chunk", eventQueueSize + 1},
		{"overflow: emit's full default event buffer", 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A slow collector is the condition under which the queue
			// overflows — and the condition this library exists to measure.
			exp := &capturingLogExporter{delay: 2 * time.Millisecond}
			sink := newProviderSink(exp, defaultResource())
			if err := sink.emit(context.Background(), outcomes(c.count)); err != nil {
				t.Fatalf("emit: %v", err)
			}
			if err := sink.Shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			if got := len(exp.all()); got != c.count {
				t.Errorf("delivered %d of %d outcomes — the queue overwrote records with no error anywhere", got, c.count)
			}
		})
	}
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

// TestConcurrentEmitLosesNothing pins the capacity guarantee across calls, not
// just within one. The queue belongs to the sink, so overlapping ExportEvents
// calls would put several times its capacity in flight and overflow it. The
// emit package reaches that: emit.Std.Flush releases its own lock before
// calling the exporter, and its background ticker can race a caller-driven
// Flush.
//
// This fixture discriminates only when the producer outruns the exporter's
// drain, which the race detector prevents: under -race it passes even with
// the lock removed. CI runs plain go test, where it does kill that mutant.
func TestConcurrentEmitLosesNothing(t *testing.T) {
	const callers, per = 4, eventQueueSize
	exp := &capturingLogExporter{delay: 2 * time.Millisecond}
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
	if got, want := len(exp.all()), callers*per; got != want {
		t.Errorf("delivered %d of %d outcomes across %d concurrent callers — the shared queue overflowed with no error returned", got, want, callers)
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
	got := recs[0].Resource()
	attrs := map[string]string{}
	for _, kv := range got.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["service.name"] != "shortfall" {
		t.Errorf("event-leg service.name = %q, want shortfall — the log provider fell back to its own default", attrs["service.name"])
	}
	inst := attrs["service.instance.id"]
	if inst == "" {
		t.Fatal("event-leg records carry no service.instance.id, so events and metrics disagree about the writer")
	}
	if !strings.Contains(inst, strconv.Itoa(os.Getpid())) {
		t.Errorf("event-leg service.instance.id = %q, want it to name this process (pid %d)", inst, os.Getpid())
	}
}
