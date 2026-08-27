// Package prometheus is a metrics-only shortfall exporter: it maps the
// bounded biz_* metric families (ADR-0004) onto native
// prometheus/client_golang collectors, so a Prometheus shop scrapes
// business impact from the same /metrics endpoint as everything else. It is
// a nested module: a user who never touches Prometheus does not pull
// client_golang into their build.
//
// It is honestly metrics-only: Capabilities reports Events=false. Amounts
// and ids ride on OUTCOME EVENTS, which Prometheus has no place for, so the
// customers leg is answered from an event sink (OTLP, Loki, ...), never
// from here — the engine reports it NotAvailable rather than silently
// empty. Pairing this exporter with an event exporter gives both.
//
// Two consequences of Prometheus's pull model are load-bearing and
// deliberate:
//   - Counter families are CUMULATIVE. The emit layer produces delta
//     points; this exporter Adds each delta to the counter, so a scrape
//     reads the running total. biz_inflight_value is a gauge Set to the
//     level observed at the point's time.
//   - Sample timestamps are NOT preserved. Prometheus text exposition
//     carries no per-sample time; the server stamps at scrape. Unlike the
//     OTLP exporter, a batch delayed by an incident is seen at scrape time,
//     not at the outcome's time. Deployments that need money pinned to
//     observation time must also run an event exporter.
package prometheus

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// scope namespaces nothing: the family names are the ADR-0004 names exactly,
// so a dashboard query is identical across exporters.

// Fixed per-family label orders (ADR-0004). Order is the contract:
// WithLabelValues is positional, so these slices must never be reordered.
var (
	valueLabels    = []string{"flow", "stage", "outcome", "currency", "kind", "segment"}
	txnLabels      = []string{"flow", "stage", "outcome", "currency", "segment"}
	inflightLabels = []string{"flow", "stage", "age_bucket", "currency"}
	providerLabels = []string{"provider", "op", "outcome"}
	droppedLabels  = []string{"reason"}
)

// Exporter implements emit.Exporter over prometheus/client_golang. It owns
// a set of collectors registered into a Registerer; a scrape of that
// registry's Gatherer renders the current impact.
type Exporter struct {
	valueTotal    *prometheus.CounterVec
	txnTotal      *prometheus.CounterVec
	inflightValue *prometheus.GaugeVec
	providerCalls *prometheus.CounterVec
	droppedEvents *prometheus.CounterVec

	registerer prometheus.Registerer
	gatherer   prometheus.Gatherer
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures registration.
type Options struct {
	registerer prometheus.Registerer
	gatherer   prometheus.Gatherer
}

// WithRegisterer registers the collectors into an existing registry (and
// scrapes it through the matching Gatherer) instead of a private one — use
// it to share the process's default /metrics endpoint.
func WithRegisterer(r prometheus.Registerer, g prometheus.Gatherer) func(*Options) {
	return func(o *Options) { o.registerer, o.gatherer = r, g }
}

// New builds the exporter and registers its collectors. With no options it
// owns a private registry; read it back with Gatherer for an http handler.
func New(opts ...func(*Options)) (*Exporter, error) {
	o := Options{}
	for _, f := range opts {
		f(&o)
	}
	if o.registerer == nil {
		reg := prometheus.NewRegistry()
		o.registerer, o.gatherer = reg, reg
	}

	e := &Exporter{
		valueTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "biz_value_total",
			Help: "Cumulative realized business value by flow/stage/outcome (minor currency units; estimates excluded).",
		}, valueLabels),
		txnTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "biz_txn_total",
			Help: "Cumulative count of transactions by flow/stage/outcome.",
		}, txnLabels),
		inflightValue: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "biz_inflight_value",
			Help: "In-flight (deferred) business value by flow/stage/age_bucket (minor currency units).",
		}, inflightLabels),
		providerCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "biz_provider_calls_total",
			Help: "Cumulative count of downstream provider calls by provider/op/outcome.",
		}, providerLabels),
		droppedEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "biz_dropped_events_total",
			Help: "Cumulative count of outcome events dropped by the emitter, by reason (ADR-0002).",
		}, droppedLabels),
		registerer: o.registerer,
		gatherer:   o.gatherer,
	}

	for _, c := range e.collectors() {
		if err := o.registerer.Register(c); err != nil {
			return nil, fmt.Errorf("prometheus: register: %w", err)
		}
	}
	return e, nil
}

func (e *Exporter) collectors() []prometheus.Collector {
	return []prometheus.Collector{e.valueTotal, e.txnTotal, e.inflightValue, e.providerCalls, e.droppedEvents}
}

// Gatherer exposes the registry to build an http handler:
//
//	promhttp.HandlerFor(exp.Gatherer(), promhttp.HandlerOpts{})
func (e *Exporter) Gatherer() prometheus.Gatherer { return e.gatherer }

// Capabilities: metrics only, honestly. Retention is the Prometheus
// server's, unknown here, so the history weeks are 0.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: false}
}

// ExportMetrics applies each point to its family: a counter delta is Added
// (Prometheus counters are cumulative), a gauge level is Set. An unknown
// family or a negative counter delta is a defect and surfaces as an error
// rather than a silent drop or a panic.
func (e *Exporter) ExportMetrics(_ context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	for _, p := range batch {
		switch p.Name {
		case "biz_value_total":
			if err := addCounter(e.valueTotal, valueLabels, p); err != nil {
				return err
			}
		case "biz_txn_total":
			if err := addCounter(e.txnTotal, txnLabels, p); err != nil {
				return err
			}
		case "biz_provider_calls_total":
			if err := addCounter(e.providerCalls, providerLabels, p); err != nil {
				return err
			}
		case "biz_dropped_events_total":
			if err := addCounter(e.droppedEvents, droppedLabels, p); err != nil {
				return err
			}
		case "biz_inflight_value":
			e.inflightValue.WithLabelValues(orderedValues(p.Labels, inflightLabels)...).Set(float64(p.Value))
		default:
			return fmt.Errorf("prometheus: unknown metric family %q", p.Name)
		}
	}
	return nil
}

func addCounter(vec *prometheus.CounterVec, labels []string, p emit.MetricPoint) error {
	if p.Value < 0 {
		return fmt.Errorf("prometheus: negative delta %d for counter family %q — counters only increase", p.Value, p.Name)
	}
	vec.WithLabelValues(orderedValues(p.Labels, labels)...).Add(float64(p.Value))
	return nil
}

// orderedValues pulls label values out of the point's map in the family's
// fixed order. A label the point omits becomes the empty string — the emit
// layer already applied ADR-0004's fallbacks (unregistered flow/stage,
// dropped segment), so an absent key here means "no value", never a bug to
// hide.
func orderedValues(labels map[string]string, order []string) []string {
	out := make([]string, len(order))
	for i, name := range order {
		out[i] = labels[name]
	}
	return out
}

// ExportEvents is a no-op: this exporter declares Events=false and keeps
// that promise by delivering nothing (returning nil, never blocking the
// caller). Outcome events belong on an event exporter.
func (e *Exporter) ExportEvents(context.Context, []biz.Outcome) error {
	return nil
}

// Shutdown is a no-op. Prometheus is scrape-based: there is nothing buffered
// to flush, and unregistering here would DELETE already-recorded series
// before a final scrape — losing data, the opposite of a flush.
func (e *Exporter) Shutdown(context.Context) error { return nil }
