// Package statsd is a metrics-only shortfall exporter for the StatsD family.
// In DogStatsD mode (the default) the ADR-0004 label sets ride as tags; in
// plain-StatsD mode — which has no tags — labels are encoded positionally
// into the metric name and a one-time warning says what that costs. It is a
// nested module whose own code imports only net + stdlib over the shortfall
// core — it wraps no StatsD client library (the core's transitive deps still
// appear in go.mod as indirect).
//
// Honestly metrics-only: Capabilities reports Events=false. StatsD has no
// place for per-transaction amounts and ids, so the customers leg is
// answered from an event sink, not here.
//
// Two protocol facts are load-bearing:
//   - Counters (biz_value_total, biz_txn_total, biz_provider_calls_total,
//     biz_dropped_events_total) send delta increments ("|c"); the StatsD
//     server accumulates them, matching the emit layer's delta points.
//     biz_inflight_value is a gauge ("|g") set to the observed level.
//   - StatsD carries no per-sample timestamp; the server stamps at receipt.
//     For the gauge that means a stale sample arriving after a fresher one
//     would set a stale level, so — like the Prometheus exporter — this
//     exporter drops a gauge sample older than the last one it sent for that
//     series (emit's order-by-At contract).
package statsd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// Format selects the wire encoding.
type Format int

const (
	// DogStatsD encodes labels as tags: name:value|type|#k:v,k:v.
	DogStatsD Format = iota
	// PlainStatsD has no tags; labels are encoded into the name.
	PlainStatsD
)

// Fixed per-family label orders (ADR-0004). Order is the contract — plain
// StatsD encodes values positionally, so reordering would silently change
// every metric name.
var (
	valueLabels    = []string{"flow", "stage", "outcome", "currency", "kind", "segment"}
	txnLabels      = []string{"flow", "stage", "outcome", "currency", "segment"}
	inflightLabels = []string{"flow", "stage", "age_bucket", "currency"}
	providerLabels = []string{"provider", "op", "outcome"}
	droppedLabels  = []string{"reason"}
)

func labelsFor(name string) []string {
	switch name {
	case "biz_value_total":
		return valueLabels
	case "biz_txn_total":
		return txnLabels
	case "biz_inflight_value", "biz_inflight_count":
		return inflightLabels
	case "biz_provider_calls_total":
		return providerLabels
	case "biz_dropped_events_total":
		return droppedLabels
	default:
		return nil
	}
}

// isGauge reports whether a family is a gauge (a level) rather than a
// counter (a delta).
func isGauge(name string) bool {
	return name == "biz_inflight_value" || name == "biz_inflight_count"
}

// sink writes one metric line. writerSink is line-delimited (files, tests);
// packetSink sends one UDP datagram per line.
type sink interface {
	writeMetric(line string) error
	Close() error
}

// Exporter implements emit.Exporter over StatsD.
type Exporter struct {
	format Format
	logger *slog.Logger

	mu   sync.Mutex
	sink sink
	// inflightAt is the last At applied to each gauge series (see package
	// doc): a stale sample must not overwrite a fresher level.
	inflightAt map[string]time.Time
	warnOnce   sync.Once
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	format  Format
	logger  *slog.Logger
	sink    sink
	dialErr error // set by WithAddress if the UDP dial failed
}

// WithWriter sends metric lines to w (newline-delimited). Mutually
// exclusive with WithAddress; the last one set wins.
func WithWriter(w io.Writer) func(*Options) {
	return func(o *Options) { o.sink = &writerSink{w: w} }
}

// WithFormat selects DogStatsD (default) or PlainStatsD.
func WithFormat(f Format) func(*Options) { return func(o *Options) { o.format = f } }

// WithLogger sets the logger for the plain-StatsD lossiness warning.
func WithLogger(l *slog.Logger) func(*Options) { return func(o *Options) { o.logger = l } }

// New builds the exporter. Provide a destination with WithAddress or
// WithWriter; with neither, lines go to a discard sink (a misconfiguration
// worth catching in review, but never a panic).
func New(opts ...func(*Options)) (*Exporter, error) {
	o := Options{format: DogStatsD, logger: slog.Default()}
	for _, f := range opts {
		f(&o)
	}
	if o.dialErr != nil {
		return nil, fmt.Errorf("statsd: dial: %w", o.dialErr)
	}
	if o.sink == nil {
		o.sink = &writerSink{w: io.Discard}
	}
	return &Exporter{
		format:     o.format,
		logger:     o.logger,
		sink:       o.sink,
		inflightAt: map[string]time.Time{},
	}, nil
}

// Capabilities: metrics only, honestly. Retention is the StatsD backend's.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: false}
}

// ExportMetrics encodes each point and writes it. An unknown family or a
// negative counter delta surfaces as an error, never a silent drop.
func (e *Exporter) ExportMetrics(_ context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range batch {
		labels := labelsFor(p.Name)
		if labels == nil {
			return fmt.Errorf("statsd: unknown metric family %q", p.Name)
		}
		if !isGauge(p.Name) && p.Value < 0 {
			return fmt.Errorf("statsd: negative delta %d for counter family %q", p.Value, p.Name)
		}
		if isGauge(p.Name) && e.staleGauge(p, labels) {
			continue
		}
		if err := e.sink.writeMetric(e.encode(p, labels)); err != nil {
			return fmt.Errorf("statsd: write: %w", err)
		}
	}
	return nil
}

// staleGauge reports whether p is older than the last gauge sample sent for
// its series; if not, it records p.At as the new latest. Caller holds e.mu.
func (e *Exporter) staleGauge(p emit.MetricPoint, labels []string) bool {
	key := p.Name + "\x00" + strings.Join(orderedValues(p.Labels, labels), "\x00")
	if last, ok := e.inflightAt[key]; ok && p.At.Before(last) {
		return true
	}
	e.inflightAt[key] = p.At
	return false
}

// encode renders one point in the configured wire format.
func (e *Exporter) encode(p emit.MetricPoint, labels []string) string {
	typ := "c"
	if isGauge(p.Name) {
		typ = "g"
	}
	if e.format == PlainStatsD {
		e.warnOnce.Do(func() {
			e.logger.Warn("statsd: plain StatsD has no tags; ADR-0004 labels are encoded positionally into the metric name — label keys are lost and dashboards must decode by position. Use DogStatsD to keep tags.",
				"metric", p.Name)
		})
		name := p.Name
		for _, v := range orderedValues(p.Labels, labels) {
			name += "." + sanitizePlain(v)
		}
		return fmt.Sprintf("%s:%d|%s", name, p.Value, typ)
	}
	// DogStatsD: tags in fixed label order, sanitized.
	tags := make([]string, 0, len(labels))
	for _, k := range labels {
		tags = append(tags, sanitizeTag(k)+":"+sanitizeTag(p.Labels[k]))
	}
	sort.Strings(tags) // stable output regardless of label-slice order
	return fmt.Sprintf("%s:%d|%s|#%s", p.Name, p.Value, typ, strings.Join(tags, ","))
}

// ExportEvents is a no-op: Events=false, kept honestly (nothing delivered).
func (e *Exporter) ExportEvents(context.Context, []biz.Outcome) error { return nil }

// Shutdown closes the sink (flushes a writer, closes a UDP conn).
func (e *Exporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.sink.Close(); err != nil {
		return fmt.Errorf("statsd: shutdown: %w", err)
	}
	return nil
}

func orderedValues(labels map[string]string, order []string) []string {
	out := make([]string, len(order))
	for i, name := range order {
		out[i] = labels[name]
	}
	return out
}

// sanitizeTag strips the StatsD/DogStatsD reserved bytes from a tag key or
// value (|, comma, colon, whitespace, newline) so a value can never break
// the line framing.
func sanitizeTag(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '|', ',', ':', '\n', '\r', ' ', '\t', '@', '#':
			return '_'
		default:
			return r
		}
	}, s)
}

// sanitizePlain strips bytes that are structural in a plain-StatsD name (dot
// is the segment separator; colon/pipe frame the value) so an encoded label
// value cannot inject a segment or break framing.
func sanitizePlain(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', ':', '|', '\n', '\r', ' ', '\t', '@', '#', ',':
			return '_'
		default:
			return r
		}
	}, s)
}
