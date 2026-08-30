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
	"sync/atomic"

	"go.opentelemetry.io/otel"
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
// discards records with no error anywhere the caller can observe. It is not tied
// to emit's larger event buffer: chunking already makes that size irrelevant, and matching it would hold 10k records in
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
	// res is the resource this sink was built from. The provider does not
	// expose it, and a test that cannot see it cannot tell whether New
	// handed both legs the same value.
	res *resource.Resource

	// mu serialises this sink's emit. The queue is per-sink, not per-call, so
	// two overlapping ExportEvents calls would put twice its capacity in
	// flight and overflow it. The emit package reaches that: emit.Std.Flush
	// releases its own lock before calling the exporter, and its background
	// ticker can race a caller-driven Flush.
	mu sync.Mutex
	// stopped is guarded by mu, and that is the whole point of it. The
	// Exporter-level atomic makes a post-Shutdown export fail, but a caller
	// already past that check can still be waiting for mu when Shutdown
	// stops the provider — and a stopped provider discards the record and
	// answers ForceFlush with nil, so emit would report success for a batch
	// nobody received. Deciding under the same lock emit holds is what makes
	// the two operations ordered instead of merely racing.
	stopped bool
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
		res:      res,
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
	if s.stopped {
		return ErrShutdown
	}
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

// Shutdown marks the sink stopped under mu, then stops the provider. An
// emit already holding mu finishes and delivers; one still waiting for it
// wakes to stopped and errors. The provider is stopped outside the lock —
// no emit can run against it once stopped is set, so holding mu across the
// SDK call would only make Shutdown wait longer.
func (s *providerSink) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	return s.provider.Shutdown(ctx)
}

// ErrShutdown is returned by ExportMetrics and ExportEvents once Shutdown
// has run. Shutdown is terminal here: the log provider cannot be restarted,
// and a post-Shutdown export delivers nothing. Reporting that as success
// would be the silent drop ADR-0002 forbids, so it is an error the caller
// can test for — emit.Std.Flush turns it into
// biz_dropped_events_total{reason=export}.
var ErrShutdown = errors.New("otlp: exporter is shut down")

// Exporter implements emit.Exporter over OTLP.
type Exporter struct {
	metrics  metricPusher
	logs     eventSink
	resource *resource.Resource

	// shutdown is read by both legs so they answer a post-Shutdown export
	// identically. Leaving it to the vendored SDKs does not: otlpmetrichttp
	// swaps in a client that errors, while sdklog's stopped provider
	// discards the record and answers ForceFlush with nil. Zero value is
	// usable because newWith and the conformance harness build Exporter as
	// a struct literal.
	shutdown atomic.Bool
}

var _ emit.Exporter = (*Exporter)(nil)

// Options carries otel exporter options for each signal.
type Options struct {
	metric   []metricexp.Option
	log      []logexp.Option
	resource *resource.Resource
}

// WithResource sets the resource carried by both signals — metric points and
// log records alike. It is merged with resource.Environment() rather than
// replacing it, so OTEL_RESOURCE_ATTRIBUTES reaches both legs, and this
// resource wins every key they share: OTEL_SERVICE_NAME cannot rename a
// writer that named itself.
//
// The default names this service and gives it a per-process instance id,
// which is what keeps replicas off each other's gauge series; an override
// must still distinguish one writer from another.
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

// resolveResource is the single place a resource becomes the one both legs
// carry. sdklog.WithResource merges resource.Environment() into whatever it
// is given, while the metric leg hand-builds its ResourceMetrics and never
// touches the metric SDK — so without this, OTEL_RESOURCE_ATTRIBUTES reached
// events only, and the two legs disagreed about the writer's context.
//
// Argument order is the whole contract: resource.Merge is last-wins, so res
// must be second for the explicit resource to beat the environment. Reversed,
// OTEL_SERVICE_NAME silently renames the writer and no error is returned.
//
// resource.Environment() is always schemaless, so the merge cannot hit
// ErrSchemaURLConflict and res's schema URL survives. resource.Default()
// would NOT be safe here: it carries a different semconv version and
// conflicts, which wipes the schema URL to "". On the error path we match
// sdklog.WithResource and hand the merged value back — Merge returns a
// usable resource even when it errors, and diverging here would trade one
// asymmetry for another.
func resolveResource(res *resource.Resource) *resource.Resource {
	merged, err := resource.Merge(resource.Environment(), res)
	if err != nil {
		// Unreachable while the first argument is Environment(): Merge errors
		// only on ErrSchemaURLConflict, which needs both schema URLs non-empty,
		// and Environment() is always schemaless. Handled rather than ignored
		// so a future SDK change surfaces through otel's handler instead of
		// being swallowed — Merge returns a usable resource either way, which
		// is why merged is returned unconditionally.
		otel.Handle(err)
	}
	return merged
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
	o.resource = resolveResource(o.resource)
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
	return &Exporter{metrics: m, logs: l, resource: resolveResource(defaultResource())}
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
	// After the empty-batch return, never before: an empty batch drops
	// nothing, so there is nothing to be loud about.
	if e.shutdown.Load() {
		return ErrShutdown
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
	if e.shutdown.Load() {
		return ErrShutdown
	}
	return e.logs.emit(ctx, batch)
}

// Shutdown flushes and closes both exporters. Both always run — a failure
// on one leg must not skip the other — and both errors are joined, since a
// log-leg flush failure can mean dropped outcome data and must not be
// masked by a metric-leg error.
func (e *Exporter) Shutdown(ctx context.Context) error {
	e.shutdown.Store(true)
	mErr := e.metrics.Shutdown(ctx)
	lErr := e.logs.Shutdown(ctx)
	return errors.Join(mErr, lErr)
}
