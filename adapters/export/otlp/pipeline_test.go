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
	// delay simulates a collector that cannot keep up. It is what makes the
	// overflow reachable: with an instant exporter the batch processor drains
	// as fast as a caller can fill it and no record is ever overwritten, so a
	// test using one proves nothing about the queue.
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

// TestNoOutcomeIsLostToQueueOverflow is the regression test for the defect
// this module shipped with: the log SDK's batch queue drops its OLDEST record
// on overflow, reports it through no channel the caller can see (Emit returns
// nothing, ForceFlush reports only export failures), and emit's default event
// buffer is five times the queue's default size. A batch larger than the
// queue therefore lost outcome events silently — realized loss and customer
// impact both read from these — while every layer reported success. ADR-0002
// makes a silent drop a defect.
func TestNoOutcomeIsLostToQueueOverflow(t *testing.T) {
	cases := []struct {
		name  string
		count int
	}{
		{"well under the queue", 10},
		{"exactly the queue", eventQueueSize},
		{"one over the queue", eventQueueSize + 1},
		{"emit's full default event buffer", 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A slow collector is the condition under which the queue
			// overflows — and the condition this library exists to measure.
			exp := &capturingLogExporter{delay: 2 * time.Millisecond}
			sink := newProviderSink(exp)
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
	sink := newProviderSink(exp)
	if err := sink.emit(context.Background(), outcomes(3)); err == nil {
		t.Fatal("want an error, got nil — a failed event export must not report success")
	}
}

// TestTraceLinking covers providerSink.emit's branching, which previously ran
// only under the integration build tag and so was never exercised in CI.
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
			sink := newProviderSink(exp)
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
				// The sampled flag is set deliberately: backends that gate
				// log-to-trace correlation on it drop the link otherwise.
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
	sink := newProviderSink(exp)
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

// TestUnknownMetricFamilyIsRejected matches the guarantee the three sibling
// exporters already enforce and docs/adapters.md states repo-wide. Shipping
// an unrecognised family as a monotonic counter is worse than dropping it: a
// mistyped or newly added LEVEL family would be summed by the backend.
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
		attrs[string(kv.Key)] = kv.Value.Emit()
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
