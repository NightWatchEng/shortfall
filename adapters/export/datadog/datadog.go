// Package datadog exports shortfall signals to Datadog: the bounded biz_*
// metric families to the v1 metrics series API and outcomes to the v2 logs
// intake. It is a thin HTTP batcher over adapters/httpbatch (retry/backoff);
// this package maps payloads and holds two clients, one per intake endpoint.
//
// Nested module — a non-Datadog user pulls neither this nor its plumbing.
// Both signals are honest (Capabilities Metrics+Events). Metric tags and log
// ddtags carry only the ADR-0004 bounded dimensions; amounts and ids ride in
// the log message, never as a metric tag or a log tag. Metric points keep
// their own timestamp (v1 series is timestamped), so a delayed batch is not
// restamped to now.
package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/NightWatchEng/shortfall/adapters/httpbatch"
	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// Fixed per-family tag sets (ADR-0004).
var familyTags = map[string][]string{
	"biz_value_total":          {"flow", "stage", "outcome", "currency", "kind", "segment"},
	"biz_txn_total":            {"flow", "stage", "outcome", "currency", "segment"},
	"biz_inflight_value":       {"flow", "stage", "age_bucket", "currency"},
	"biz_provider_calls_total": {"provider", "op", "outcome"},
	"biz_dropped_events_total": {"reason"},
}

// logTagKeys are the bounded dimensions used as log ddtags; everything else
// (amounts, ids) rides in the message attributes.
var logTagKeys = []string{"flow", "stage", "outcome"}

const maxBatch = 500

// Exporter implements emit.Exporter over Datadog's HTTP intakes.
type Exporter struct {
	metrics *httpbatch.Client
	logs    *httpbatch.Client
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	site            string
	metricsEndpoint string
	logsEndpoint    string
	httpOpts        []httpbatch.Option
}

// WithSite selects the Datadog site (e.g. "datadoghq.eu"); default
// "datadoghq.com". Ignored if explicit endpoints are set.
func WithSite(site string) func(*Options) { return func(o *Options) { o.site = site } }

// WithMetricsEndpoint overrides the full metrics series URL (tests, proxies).
func WithMetricsEndpoint(url string) func(*Options) {
	return func(o *Options) { o.metricsEndpoint = url }
}

// WithLogsEndpoint overrides the full logs intake URL (tests, proxies).
func WithLogsEndpoint(url string) func(*Options) { return func(o *Options) { o.logsEndpoint = url } }

// WithHTTPClient injects the HTTP doer for BOTH intakes (tests, transports).
func WithHTTPClient(d httpbatch.Doer) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, httpbatch.WithHTTPClient(d)) }
}

// WithBatcherOptions passes options to both underlying batchers (e.g. retry).
func WithBatcherOptions(opts ...httpbatch.Option) func(*Options) {
	return func(o *Options) { o.httpOpts = append(o.httpOpts, opts...) }
}

// New builds a Datadog exporter authenticated with apiKey.
func New(apiKey string, opts ...func(*Options)) *Exporter {
	o := Options{site: "datadoghq.com"}
	for _, f := range opts {
		f(&o)
	}
	if o.metricsEndpoint == "" {
		o.metricsEndpoint = "https://api." + o.site + "/api/v1/series"
	}
	if o.logsEndpoint == "" {
		o.logsEndpoint = "https://http-intake.logs." + o.site + "/api/v2/logs"
	}
	httpOpts := append([]httpbatch.Option{httpbatch.WithHeader("DD-API-KEY", apiKey)}, o.httpOpts...)
	return &Exporter{
		metrics: httpbatch.New(o.metricsEndpoint, httpOpts...),
		logs:    httpbatch.New(o.logsEndpoint, httpOpts...),
	}
}

// Capabilities: both signals; retention is Datadog's.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: true}
}

// series payload (Datadog v1).
type seriesPayload struct {
	Series []seriesItem `json:"series"`
}
type seriesItem struct {
	Metric string       `json:"metric"`
	Points [][2]float64 `json:"points"`
	Type   string       `json:"type"`
	Tags   []string     `json:"tags"`
}

// ExportMetrics maps each point to a v1 series item (count for counters,
// gauge for the level), keeping the point's own second-resolution timestamp.
func (e *Exporter) ExportMetrics(ctx context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	items := make([]seriesItem, 0, len(batch))
	for _, p := range batch {
		tagKeys, ok := familyTags[p.Name]
		if !ok {
			return fmt.Errorf("datadog: unknown metric family %q", p.Name)
		}
		typ := "count"
		if p.Name == "biz_inflight_value" {
			typ = "gauge"
		}
		items = append(items, seriesItem{
			Metric: p.Name,
			Points: [][2]float64{{float64(p.At.Unix()), float64(p.Value)}},
			Type:   typ,
			Tags:   tagsFrom(p.Labels, tagKeys),
		})
	}
	for start := 0; start < len(items); start += maxBatch {
		end := start + maxBatch
		if end > len(items) {
			end = len(items)
		}
		body, err := json.Marshal(seriesPayload{Series: items[start:end]})
		if err != nil {
			return fmt.Errorf("datadog: marshal series: %w", err)
		}
		if err := e.metrics.Post(ctx, "application/json", body); err != nil {
			return err
		}
	}
	return nil
}

// logItem is one Datadog log (v2 intake accepts an array of these).
type logItem struct {
	DDSource string `json:"ddsource"`
	Service  string `json:"service"`
	DDTags   string `json:"ddtags"`
	Message  string `json:"message"`
}

// ExportEvents maps each outcome to a Datadog log whose message is the full
// outcome JSON (amounts and ids included) and whose ddtags are the bounded
// dimensions only.
func (e *Exporter) ExportEvents(ctx context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	items := make([]logItem, 0, len(batch))
	for _, o := range batch {
		msg, err := outcomeMessage(o)
		if err != nil {
			return err
		}
		items = append(items, logItem{
			DDSource: "shortfall",
			Service:  "shortfall",
			DDTags:   logTags(o),
			Message:  msg,
		})
	}
	for start := 0; start < len(items); start += maxBatch {
		end := start + maxBatch
		if end > len(items) {
			end = len(items)
		}
		body, err := json.Marshal(items[start:end])
		if err != nil {
			return fmt.Errorf("datadog: marshal logs: %w", err)
		}
		if err := e.logs.Post(ctx, "application/json", body); err != nil {
			return err
		}
	}
	return nil
}

// tagsFrom builds sorted "k:v" tags from the family's fixed key set.
func tagsFrom(labels map[string]string, keys []string) []string {
	tags := make([]string, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, k+":"+labels[k])
	}
	sort.Strings(tags)
	return tags
}

// logTags builds the bounded ddtags string for an outcome.
func logTags(o biz.Outcome) string {
	vals := map[string]string{"flow": o.VC.Flow, "stage": o.Stage, "outcome": string(o.Result)}
	tags := make([]string, 0, len(logTagKeys))
	for _, k := range logTagKeys {
		tags = append(tags, k+":"+vals[k])
	}
	sort.Strings(tags)
	out := ""
	for i, t := range tags {
		if i > 0 {
			out += ","
		}
		out += t
	}
	return out
}

func outcomeMessage(o biz.Outcome) (string, error) {
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
		"timestamp":        o.At.UnixMilli(),
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
		return "", fmt.Errorf("datadog: marshal message: %w", err)
	}
	return string(b), nil
}

// Shutdown is a no-op: each Export posts synchronously.
func (e *Exporter) Shutdown(context.Context) error { return nil }
