// Package otlp exports shortfall's two signal kinds over OpenTelemetry:
// bounded metrics through the OTLP metric exporter and per-transaction
// outcome events through the OTLP Log exporter (event.name=biz.outcome),
// so one integration reaches any backend an OpenTelemetry Collector fans
// out to.
//
// It is a nested module: a Prometheus-only user never pulls the otel
// OTLP/gRPC stack into their build. Per ADR-0002 the experimental
// go.opentelemetry.io/otel/sdk/log dependency is isolated here and
// nowhere in the core.
//
// Metric temporality is delta for the counter families (matching
// emit.MetricPoint's delta semantics) and gauge for the two in-flight
// families (ADR-0012); each point is stamped with its own observation
// time, never flush time.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	logexp "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	metricexp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// eventQueueSize bounds the log SDK's record queue and the chunk size
// providerSink.emit hands it. The two must agree: an overflowing queue
// discards records with no error anywhere the caller can observe. It is
// deliberately not tied to emit's larger event buffer — chunking already
// makes that size irrelevant, and matching it would hold 10k records in
// memory before any drain. Setting it explicitly also overrides
// OTEL_BLRP_MAX_QUEUE_SIZE, which the chunking contract depends on.
const eventQueueSize = 2048

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

	// mu serialises emit. The queue below is per-sink, not per-call, so two
	// overlapping ExportEvents calls would put twice its capacity in flight
	// and overflow it — emit releases its own lock before calling the
	// exporter, and its ticker can race a caller-driven Flush.
	mu sync.Mutex
}

// newProviderSink builds the real event pipeline over an otel log exporter.
// The queue is sized explicitly rather than left to the SDK default, because
// emit's chunking guarantee below is stated in terms of it.
func newProviderSink(exp sdklog.Exporter, res *resource.Resource) *providerSink {
	// WithAttributeCountLimit(-1) disables the attribute cap so every biz.*
	// field survives however many an outcome carries. The SDK default (128)
	// clears today's ~12, but is a silent cliff — pin the safe value rather
	// than lean on a default that could change under us (ADR-0002).
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp, sdklog.WithMaxQueueSize(eventQueueSize))),
		sdklog.WithAttributeCountLimit(-1),
		sdklog.WithResource(res),
	)
	return &providerSink{
		provider: provider,
		logger:   provider.Logger("github.com/NightWatchEng/shortfall"),
	}
}

// emit ships outcomes in chunks no larger than the processor's queue,
// flushing each chunk before the next. On overflow the queue overwrites its
// oldest record and reports it through no channel the caller can reach: Emit
// returns nothing and ForceFlush surfaces only export failures. Chunking
// under a lock is what keeps it from overflowing, which ADR-0002 requires —
// a dropped outcome must be counted, never silent.
func (s *providerSink) emit(ctx context.Context, batch []biz.Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for start := 0; start < len(batch); start += eventQueueSize {
		end := min(start+eventQueueSize, len(batch))
		if err := s.emitChunk(ctx, batch[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *providerSink) emitChunk(ctx context.Context, batch []biz.Outcome) error {
	for _, o := range batch {
		emitCtx := ctx
		if o.TraceID != "" {
			if tid, err := oteltrace.TraceIDFromHex(o.TraceID); err == nil {
				// TraceID only: an outcome references a transaction's trace,
				// not a specific span, so SpanID stays zero. The Sampled flag
				// is set so backends that gate log-to-trace correlation on it
				// still link the event (an unset flag reads as "unsampled").
				sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
					TraceID:    tid,
					TraceFlags: oteltrace.FlagsSampled,
				})
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
	metrics  metricPusher
	logs     eventSink
	resource *resource.Resource
}

var _ emit.Exporter = (*Exporter)(nil)

// Options carries otel exporter options for each signal.
type Options struct {
	metric   []metricexp.Option
	log      []logexp.Option
	resource *resource.Resource
}

// WithResource overrides the resource carried by both signals — metric
// points and log records alike. The default names this service and gives it
// a per-process instance id, which is what keeps replicas off each other's
// gauge series; an override must still distinguish one writer from another.
func WithResource(res *resource.Resource) func(*Options) {
	return func(o *Options) { o.resource = res }
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
	o := Options{resource: defaultResource()}
	for _, f := range opts {
		f(&o)
	}
	if o.resource == nil {
		o.resource = defaultResource()
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
	return &Exporter{metrics: m, logs: newProviderSink(l, o.resource), resource: o.resource}, nil
}

// newWith wires arbitrary pushers — the seam the unit tests and the
// testkit/conformance suite drive with in-memory collectors.
func newWith(m metricPusher, l eventSink) *Exporter {
	return &Exporter{metrics: m, logs: l, resource: defaultResource()}
}

// Capabilities: OTLP writes both signals; it is write-only, so read-side
// retention is unknown here (the backend owns it) and reported as 0.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: true}
}

// ExportMetrics translates the batch to OTLP metric data (delta sums +
// gauges, per-point timestamps) and ships it. An unrecognised biz_* family
// surfaces as an error rather than being shipped under a guessed kind.
func (e *Exporter) ExportMetrics(ctx context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	rm, err := buildResourceMetrics(batch, e.resource)
	if err != nil {
		return err
	}
	return e.metrics.Export(ctx, rm)
}

// ExportEvents translates outcomes to OTLP log records and ships them.
func (e *Exporter) ExportEvents(ctx context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	return e.logs.emit(ctx, batch)
}

// Shutdown flushes and closes both exporters. Both always run — a failure
// on one leg must not skip the other — and both errors are joined, since a
// log-leg flush failure can mean dropped outcome data and must not be
// masked by a metric-leg error.
func (e *Exporter) Shutdown(ctx context.Context) error {
	mErr := e.metrics.Shutdown(ctx)
	lErr := e.logs.Shutdown(ctx)
	return errors.Join(mErr, lErr)
}
