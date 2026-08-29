// Package gcp exports shortfall's two signal kinds to Google Cloud: the
// bounded biz_* metric families to Cloud Monitoring as custom metrics, and
// one structured Cloud Logging entry per outcome carrying the exact amount
// and the entity/customer ids.
//
// The module depends on nothing beyond the standard library and shortfall's
// core. Cloud Monitoring is reached over its REST API through an injected
// HTTP client, so the caller brings its own credentials (an oauth2 client
// from golang.org/x/oauth2/google is the usual choice) and a GCP user never
// pulls a cloud SDK to instrument a service.
//
// The event path writes line-delimited JSON to an io.Writer — os.Stdout on
// Cloud Run, GKE, or anywhere the logging agent collects stdout — which
// Cloud Logging parses into a structured jsonPayload with no API call and no
// credentials at all. Unlike CloudWatch EMF, Cloud Logging does not extract
// metrics from log entries, so the two paths are genuinely independent:
// configured without a monitoring client the exporter reports itself
// events-only rather than pretending to ship metrics.
package gcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// Doer is the slice of *http.Client this adapter needs. The caller injects
// an authenticated client; nothing here knows how credentials are obtained.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Exporter implements emit.Exporter over Cloud Monitoring and Cloud Logging.
type Exporter struct {
	mu sync.Mutex // guards w (bufio is not concurrency-safe)
	w  *bufio.Writer

	projectID string
	mon       *monitoringClient // nil when no monitoring client is configured
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	w         io.Writer
	projectID string
	doer      Doer
	endpoint  string
	prefix    string
	resource  monitoredRes
}

// WithWriter directs Cloud Logging entries to w (default os.Stdout).
func WithWriter(w io.Writer) func(*Options) { return func(o *Options) { o.w = w } }

// WithMonitoring enables the metric path against projectID using doer for
// transport. Without it the exporter is events-only and Capabilities reports
// Metrics false, because Cloud Logging extracts no metrics from log entries.
func WithMonitoring(projectID string, doer Doer) func(*Options) {
	return func(o *Options) {
		o.projectID = projectID
		o.doer = doer
	}
}

// WithMonitoringEndpoint overrides the Cloud Monitoring base URL (default
// https://monitoring.googleapis.com), for a private endpoint or a test
// server.
func WithMonitoringEndpoint(url string) func(*Options) {
	return func(o *Options) { o.endpoint = url }
}

// WithMetricPrefix overrides the custom metric type prefix (default
// custom.googleapis.com/biz/). The family name minus its biz_ prefix is
// appended, so biz_value_total becomes custom.googleapis.com/biz/value_total.
func WithMetricPrefix(prefix string) func(*Options) {
	return func(o *Options) { o.prefix = prefix }
}

// WithResource overrides the monitored resource the metric families are
// written against. The default is a generic_task carrying a per-process
// task_id, which is what keeps each replica's cumulative totals on its own
// series; override it to describe the runtime more precisely (gce_instance,
// k8s_container, cloud_run_revision), but keep a label that distinguishes one
// writer from another or replicas will overwrite each other's running totals.
func WithResource(resourceType string, labels map[string]string) func(*Options) {
	return func(o *Options) {
		copied := make(map[string]string, len(labels))
		for k, v := range labels {
			copied[k] = v
		}
		o.resource = monitoredRes{Type: resourceType, Labels: copied}
	}
}

// New builds the exporter. With no options it writes Cloud Logging entries
// to os.Stdout and ships no metrics.
func New(opts ...func(*Options)) *Exporter {
	o := Options{w: os.Stdout, endpoint: defaultMonitoringEndpoint, prefix: defaultMetricPrefix}
	for _, f := range opts {
		f(&o)
	}
	if o.w == nil {
		o.w = os.Stdout
	}
	if o.endpoint == "" {
		o.endpoint = defaultMonitoringEndpoint
	}
	if o.prefix == "" {
		o.prefix = defaultMetricPrefix
	}
	e := &Exporter{w: bufio.NewWriter(o.w), projectID: o.projectID}
	if o.doer != nil && o.projectID != "" {
		e.mon = newMonitoringClient(o.projectID, o.endpoint, o.prefix, o.resource, o.doer)
	}
	return e
}

// Capabilities reports metrics only when a monitoring client is configured.
// Retention is the project's, unknown here, so the history weeks stay 0.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: e.mon != nil, Events: true}
}

// ExportMetrics writes the batch to Cloud Monitoring as custom time series.
// Without a monitoring client it is a no-op, matching the capability it
// declares. An unrecognised biz_* family surfaces as an error rather than
// being dropped.
func (e *Exporter) ExportMetrics(ctx context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 || e.mon == nil {
		return nil
	}
	return e.mon.export(ctx, batch)
}

// ExportEvents writes one structured Cloud Logging entry per outcome.
func (e *Exporter) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, o := range batch {
		rec, err := buildEventRecord(e.projectID, o)
		if err != nil {
			return err
		}
		if _, err := e.w.Write(rec); err != nil {
			return fmt.Errorf("gcp: write: %w", err)
		}
		if err := e.w.WriteByte('\n'); err != nil {
			return fmt.Errorf("gcp: write: %w", err)
		}
	}
	return nil
}

// Shutdown flushes the buffered writer. Buffered records are outcome data
// that must reach the log, so a flush error surfaces rather than being
// swallowed.
func (e *Exporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.w.Flush(); err != nil {
		return fmt.Errorf("gcp: flush: %w", err)
	}
	return nil
}
