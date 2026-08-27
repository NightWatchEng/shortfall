package otlp

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/NightWatchEng/shortfall/biz"
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

// countingEvents is an eventSink that tallies the outcomes it receives.
type countingEvents struct{ events int }

func (c *countingEvents) emit(_ context.Context, batch []biz.Outcome) error {
	c.events += len(batch)
	return nil
}
func (c *countingEvents) Shutdown(context.Context) error { return nil }

// otlpBackend exposes the two counters to the conformance suite.
type otlpBackend struct {
	m *countingMetric
	l *countingEvents
}

func (b otlpBackend) MetricPoints() int { return b.m.points }
func (b otlpBackend) Events() int       { return b.l.events }

// otlpHarness constructs the real OTLP Exporter over the counting backends,
// exercising the actual ExportMetrics/ExportEvents/Shutdown mapping — not a
// stand-in — so the suite judges this adapter's true behavior.
type otlpHarness struct{}

func (otlpHarness) New() (emit.Exporter, conformance.Backend) {
	m := &countingMetric{}
	l := &countingEvents{}
	return newWith(m, l), otlpBackend{m: m, l: l}
}

func TestOTLPConformance(t *testing.T) {
	conformance.RunExporter(t, otlpHarness{})
}
