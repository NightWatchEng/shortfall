// Package otlp is the default shortfall exporter: bounded metrics through
// the OpenTelemetry metric OTLP exporter and per-transaction outcome
// events through the OTLP Log exporter (event.name=biz.outcome), so one
// integration reaches any backend an OpenTelemetry Collector fans out to.
//
// It is a NESTED module: a Prometheus-only user never pulls the otel
// OTLP/gRPC stack into their build. Per ADR-0002 the experimental
// go.opentelemetry.io/otel/sdk/log dependency is isolated HERE and
// nowhere in the core.
//
// Metric temporality is DELTA for the counter families (matching
// emit.MetricPoint's delta semantics) and gauge for biz_inflight_value;
// each point is stamped with its own observation time, never flush time.
package otlp

import (
	"context"
	"fmt"

	logexp "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	metricexp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// metricPusher is the narrow slice of the otel metric exporter this
// adapter drives — an interface so a test substitutes an in-memory
// collector with no network.
type metricPusher interface {
	Export(ctx context.Context, rm *metricdata.ResourceMetrics) error
	Shutdown(ctx context.Context) error
}

// eventSink emits outcome records and shuts down. The real sink runs the
// otel Log SDK pipeline (LoggerProvider + OTLP exporter, unlimited
// attributes); a fake captures outcomes for the mapping tests.
type eventSink interface {
	emit(ctx context.Context, batch []biz.Outcome) error
	Shutdown(ctx context.Context) error
}

// providerSink is the real event pipeline. Logger.Emit links a trace
// from the emit ctx, so each outcome carrying a trace id is emitted under
// a reconstructed span context; ForceFlush ships the batch synchronously.
type providerSink struct {
	provider *sdklog.LoggerProvider
	logger   otellog.Logger
}

func (s *providerSink) emit(ctx context.Context, batch []biz.Outcome) error {
	for _, o := range batch {
		emitCtx := ctx
		if o.TraceID != "" {
			if tid, err := oteltrace.TraceIDFromHex(o.TraceID); err == nil {
				sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: tid})
				emitCtx = oteltrace.ContextWithSpanContext(ctx, sc)
			}
		}
		s.logger.Emit(emitCtx, buildRecord(o))
	}
	return s.provider.ForceFlush(ctx)
}

func (s *providerSink) Shutdown(ctx context.Context) error { return s.provider.Shutdown(ctx) }

// Exporter implements emit.Exporter over OTLP.
type Exporter struct {
	metrics metricPusher
	logs    eventSink
}

var _ emit.Exporter = (*Exporter)(nil)

// Options carries otel exporter options for each signal.
type Options struct {
	metric []metricexp.Option
	log    []logexp.Option
}

// WithMetricOptions passes options to the OTLP metric exporter
// (endpoint, TLS, headers). With none, the standard OTEL_EXPORTER_OTLP_*
// environment is read.
func WithMetricOptions(o ...metricexp.Option) func(*Options) {
	return func(op *Options) { op.metric = append(op.metric, o...) }
}

// WithLogOptions passes options to the OTLP log exporter.
func WithLogOptions(o ...logexp.Option) func(*Options) {
	return func(op *Options) { op.log = append(op.log, o...) }
}

// New builds an OTLP exporter wired to real otel HTTP exporters.
func New(ctx context.Context, opts ...func(*Options)) (*Exporter, error) {
	var o Options
	for _, f := range opts {
		f(&o)
	}
	m, err := metricexp.New(ctx, o.metric...)
	if err != nil {
		return nil, fmt.Errorf("otlp: metric exporter: %w", err)
	}
	l, err := logexp.New(ctx, o.log...)
	if err != nil {
		_ = m.Shutdown(ctx)
		return nil, fmt.Errorf("otlp: log exporter: %w", err)
	}
	// Unlimited attributes: an outcome carries ~12 biz.* fields and the
	// SDK's default limit would silently drop them (a real sdk/log
	// footgun, ADR-0002's experimental-dependency risk realized here).
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(l)),
		sdklog.WithAttributeCountLimit(-1),
	)
	sink := &providerSink{provider: provider, logger: provider.Logger("github.com/NightWatchEng/shortfall")}
	return &Exporter{metrics: m, logs: sink}, nil
}

// newWith wires arbitrary pushers — the seam the conformance/unit tests
// drive with in-memory collectors.
func newWith(m metricPusher, l eventSink) *Exporter {
	return &Exporter{metrics: m, logs: l}
}

// Capabilities: OTLP writes both signals; it is write-only, so read-side
// retention is unknown here (the backend owns it) and reported as 0.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: true}
}

// ExportMetrics translates the batch to OTLP metric data (delta sums +
// gauges, per-point timestamps) and ships it.
func (e *Exporter) ExportMetrics(ctx context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	return e.metrics.Export(ctx, buildResourceMetrics(batch))
}

// ExportEvents translates outcomes to OTLP log records and ships them.
func (e *Exporter) ExportEvents(ctx context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	return e.logs.emit(ctx, batch)
}

// Shutdown flushes and closes both exporters, returning the first error.
func (e *Exporter) Shutdown(ctx context.Context) error {
	mErr := e.metrics.Shutdown(ctx)
	lErr := e.logs.Shutdown(ctx)
	if mErr != nil {
		return mErr
	}
	return lErr
}
