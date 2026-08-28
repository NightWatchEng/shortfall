// Package loki exports shortfall outcome events to Grafana Loki's push API.
// Loki is a LOG store, not a metrics store, so this exporter is honestly
// events-only (Capabilities Events=true, Metrics=false): the engine reads
// the customers leg from here and gets its metric families from a metrics
// exporter (Prometheus, OTLP, ...).
//
// Cardinality discipline (the whole point of ADR-0004, and doubly so for
// Loki): only the BOUNDED outcome dimensions become stream LABELS — an
// unbounded label set is the classic way to melt a Loki cluster. Amounts,
// entity ids and customer ids ride in the LOG LINE, never as labels.
//
// Thin HTTP batcher over adapters/httpbatch, which supplies retry/backoff.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/NightWatchEng/shortfall/adapters/httpbatch"
	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// streamLabelKeys are the BOUNDED dimensions used as Loki stream labels.
// Everything higher-cardinality (currency, amounts, ids) stays in the line.
var streamLabelKeys = []string{"flow", "stage", "outcome"}

// Exporter implements emit.Exporter over Loki's push API.
type Exporter struct {
	client *httpbatch.Client
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	httpOpts []httpbatch.Option
}

// WithHTTPClient injects the HTTP doer (tests, custom transports).
func WithHTTPClient(d httpbatch.Doer) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, httpbatch.WithHTTPClient(d)) }
}

// WithOrgID sets Loki's multi-tenancy header (X-Scope-OrgID).
func WithOrgID(id string) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, httpbatch.WithHeader("X-Scope-OrgID", id)) }
}

// WithBatcherOptions passes options straight to the batcher (e.g. retry).
func WithBatcherOptions(opts ...httpbatch.Option) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, opts...) }
}

// New builds a Loki exporter posting to endpoint (the full push URL, e.g.
// https://loki.example/loki/api/v1/push).
func New(endpoint string, opts ...func(*Options)) *Exporter {
	o := Options{}
	for _, f := range opts {
		f(&o)
	}
	return &Exporter{client: httpbatch.New(endpoint, o.httpOpts...)}
}

// Capabilities: events only — Loki has no metrics ingest, and saying so is
// the honest thing (the engine reports the metrics-derived legs from a
// metrics exporter, not silently empty from here).
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: false, Events: true}
}

// ExportMetrics is a no-op: Loki stores logs, not metrics. Declared
// Metrics=false and kept — nothing delivered, no error.
func (e *Exporter) ExportMetrics(context.Context, []emit.MetricPoint) error { return nil }

// pushRequest is Loki's /push body.
type pushRequest struct {
	Streams []stream `json:"streams"`
}
type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// ExportEvents groups outcomes by their bounded stream labels and posts one
// Loki push with a stream per label set. The log line is the full outcome
// JSON (amounts and ids included — line, never label).
func (e *Exporter) ExportEvents(ctx context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	byLabels := map[string][][2]string{}
	labelsFor := map[string]map[string]string{}
	order := []string{}

	for _, o := range batch {
		labels := map[string]string{
			"flow":    o.VC.Flow,
			"stage":   o.Stage,
			"outcome": string(o.Result),
		}
		key := streamKey(labels)
		if _, seen := byLabels[key]; !seen {
			order = append(order, key)
			labelsFor[key] = labels
		}
		line, err := outcomeLine(o)
		if err != nil {
			return err
		}
		ts := strconv.FormatInt(o.At.UnixNano(), 10)
		byLabels[key] = append(byLabels[key], [2]string{ts, line})
	}

	sort.Strings(order) // deterministic stream order
	req := pushRequest{Streams: make([]stream, 0, len(order))}
	for _, key := range order {
		req.Streams = append(req.Streams, stream{Stream: labelsFor[key], Values: byLabels[key]})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("loki: marshal push: %w", err)
	}
	return e.client.Post(ctx, "application/json", body)
}

// streamKey is a stable key for a label set (fixed key order).
func streamKey(labels map[string]string) string {
	s := ""
	for _, k := range streamLabelKeys {
		s += k + "=" + labels[k] + ","
	}
	return s
}

// outcomeLine renders the full outcome as a JSON log line — amounts, ids and
// all the non-label fields live here.
func outcomeLine(o biz.Outcome) (string, error) {
	m := map[string]any{
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
		m["biz.segment"] = o.VC.Segment
	}
	if o.Source != "" {
		m["source"] = o.Source
	}
	if o.Err != "" {
		m["error"] = o.Err
	}
	if o.TraceID != "" {
		m["trace.id"] = o.TraceID
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("loki: marshal line: %w", err)
	}
	return string(b), nil
}

// Shutdown is a no-op: ExportEvents posts synchronously.
func (e *Exporter) Shutdown(context.Context) error { return nil }
