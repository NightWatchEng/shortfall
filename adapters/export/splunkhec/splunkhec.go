// Package splunkhec exports shortfall signals to Splunk's HTTP Event
// Collector: the bounded biz_* metric families as HEC metric events (for a
// metrics index) and outcomes as HEC log events carrying the per-transaction
// amounts and ids. It is a thin HTTP batcher over adapters/httpbatch, which
// supplies the retry/backoff; this package only maps payloads.
//
// Nested module: a non-Splunk user pulls neither this nor its plumbing. Both
// signals are honest (Capabilities Metrics+Events): HEC carries metrics and
// events on the same endpoint. Each record stamps its own time from the
// point's/outcome's At, so a delayed batch keeps money at observation time.
package splunkhec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/NightWatchEng/shortfall/adapters/httpbatch"
	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// Fixed per-family label sets (ADR-0004). Order is documentation here; HEC
// fields are a JSON object, but pinning the set is what keeps an amount or id
// off a metric.
var familyFields = map[string][]string{
	"biz_value_total":          {"flow", "stage", "outcome", "currency", "kind", "segment"},
	"biz_txn_total":            {"flow", "stage", "outcome", "currency", "segment"},
	"biz_inflight_value":       {"flow", "stage", "age_bucket", "currency"},
	"biz_inflight_count":       {"flow", "stage", "age_bucket", "currency"},
	"biz_provider_calls_total": {"provider", "op", "outcome"},
	"biz_dropped_events_total": {"reason"},
}

// maxBatch bounds events per POST so a large flush cannot exceed HEC's
// payload limit; batches beyond it are chunked.
const maxBatch = 100

// Exporter implements emit.Exporter over Splunk HEC.
type Exporter struct {
	client *httpbatch.Client
	source string
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	source   string
	httpOpts []httpbatch.Option
}

// WithSource overrides the HEC "source" field (default "shortfall").
func WithSource(s string) func(*Options) { return func(o *Options) { o.source = s } }

// WithHTTPClient injects the HTTP doer (tests, custom transports).
func WithHTTPClient(d httpbatch.Doer) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, httpbatch.WithHTTPClient(d)) }
}

// WithBatcherOptions passes options straight to the underlying batcher
// (e.g. httpbatch.WithRetry) for callers that need to tune retry/backoff.
func WithBatcherOptions(opts ...httpbatch.Option) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, opts...) }
}

// New builds a HEC exporter posting to endpoint (the full collector URL, e.g.
// https://host:8088/services/collector) authenticated with token.
func New(endpoint, token string, opts ...func(*Options)) *Exporter {
	o := Options{source: "shortfall"}
	for _, f := range opts {
		f(&o)
	}
	httpOpts := append([]httpbatch.Option{httpbatch.WithHeader("Authorization", "Splunk "+token)}, o.httpOpts...)
	return &Exporter{
		client: httpbatch.New(endpoint, httpOpts...),
		source: o.source,
	}
}

// Capabilities: HEC carries both signals; retention is Splunk's.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: true}
}

// hecTime renders a time as HEC's epoch-seconds-with-millis float.
func hecTime(unixMilli int64) float64 { return float64(unixMilli) / 1000.0 }

// ExportMetrics maps each point to a HEC metric event and posts them.
func (e *Exporter) ExportMetrics(ctx context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	lines := make([][]byte, 0, len(batch))
	for _, p := range batch {
		fields, ok := familyFields[p.Name]
		if !ok {
			return fmt.Errorf("splunkhec: unknown metric family %q", p.Name)
		}
		f := map[string]any{"metric_name": p.Name, "_value": p.Value}
		for _, k := range fields {
			f[k] = p.Labels[k]
		}
		rec := map[string]any{
			"time":   hecTime(p.At.UnixMilli()),
			"event":  "metric",
			"source": e.source,
			"fields": f,
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("splunkhec: marshal metric: %w", err)
		}
		lines = append(lines, b)
	}
	return e.postChunks(ctx, lines)
}

// ExportEvents maps each outcome to a HEC log event carrying amounts and ids.
func (e *Exporter) ExportEvents(ctx context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	lines := make([][]byte, 0, len(batch))
	for _, o := range batch {
		event := map[string]any{
			"biz.flow":         o.VC.Flow,
			"biz.stage":        o.Stage,
			"biz.outcome":      string(o.Result),
			"biz.entity.id":    o.VC.EntityID,
			"biz.customer.id":  o.VC.CustomerID,
			"biz.amount_minor": o.VC.Money.Amount,
			"biz.currency":     o.VC.Money.Currency,
			"biz.exponent":     o.VC.Money.Exponent,
			"biz.value.kind":   string(o.VC.Kind),
			"biz.amount.est":   o.VC.Estimated,
		}
		if o.VC.Segment != "" {
			event["biz.segment"] = o.VC.Segment
		}
		if o.Source != "" {
			event["source_system"] = o.Source
		}
		if o.Err != "" {
			event["error"] = o.Err
		}
		if o.TraceID != "" {
			event["trace.id"] = o.TraceID
		}
		rec := map[string]any{
			"time":       hecTime(o.At.UnixMilli()),
			"source":     e.source,
			"sourcetype": "shortfall:outcome",
			"event":      event,
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("splunkhec: marshal event: %w", err)
		}
		lines = append(lines, b)
	}
	return e.postChunks(ctx, lines)
}

// postChunks concatenates lines newline-separated (HEC accepts multiple
// events per request) and posts them in chunks of maxBatch.
func (e *Exporter) postChunks(ctx context.Context, lines [][]byte) error {
	for start := 0; start < len(lines); start += maxBatch {
		end := start + maxBatch
		if end > len(lines) {
			end = len(lines)
		}
		body := joinLines(lines[start:end])
		if err := e.client.Post(ctx, "application/json", body); err != nil {
			return err
		}
	}
	return nil
}

func joinLines(lines [][]byte) []byte {
	var out []byte
	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, l...)
	}
	return out
}

// Shutdown is a no-op: each Export posts synchronously, nothing is buffered.
func (e *Exporter) Shutdown(context.Context) error { return nil }
