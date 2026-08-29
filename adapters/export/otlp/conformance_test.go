package otlp

import (
	"context"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// countingMetric is a metricPusher that tallies the metric data points it
// receives — the backend the conformance suite reads for the metric leg.
type countingMetric struct{ points int }

func (c *countingMetric) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				c.points += len(d.DataPoints)
			case metricdata.Gauge[int64]:
				c.points += len(d.DataPoints)
			}
		}
	}
	return nil
}
func (c *countingMetric) Shutdown(context.Context) error { return nil }

// countingLogExporter is an otel log Exporter that tallies the records
// actually DELIVERED to it. The seam is deliberately here, at the outermost
// transport, rather than at the eventSink interface: substituting the sink
// would excise buildRecord, the LoggerProvider, the BatchProcessor and
// ForceFlush, and "events flush on shutdown with no loss" would reduce to
// counting what the exporter accepted — arithmetically incapable of failing,
// and blind to exactly the buffering layer the suite exists to judge.
type countingLogExporter struct {
	mu      sync.Mutex
	records int
}

func (c *countingLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	c.mu.Lock()
	c.records += len(recs)
	c.mu.Unlock()
	return nil
}
func (c *countingLogExporter) Shutdown(context.Context) error   { return nil }
func (c *countingLogExporter) ForceFlush(context.Context) error { return nil }

func (c *countingLogExporter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records
}

// otlpBackend exposes the two counters to the conformance suite.
type otlpBackend struct {
	m *countingMetric
	l *countingLogExporter
}

func (b otlpBackend) MetricPoints() int { return b.m.points }
func (b otlpBackend) Events() int       { return b.l.count() }

// otlpHarness constructs the real OTLP Exporter over counting transports:
// the real metric mapping and the real log pipeline (records, provider,
// batch processor, flush) both run, so the suite judges delivery rather than
// acceptance.
type otlpHarness struct{}

func (otlpHarness) New() (emit.Exporter, conformance.Backend) {
	m := &countingMetric{}
	l := &countingLogExporter{}
	e := &Exporter{metrics: m, logs: newProviderSink(l), resource: defaultResource()}
	return e, otlpBackend{m: m, l: l}
}

func TestOTLPConformance(t *testing.T) {
	conformance.RunExporter(t, otlpHarness{})
}
