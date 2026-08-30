// Package cloudwatch is a CloudWatch exporter built on the Embedded Metric
// Format: it writes EMF JSON records (metrics and outcome events) to an
// io.Writer — os.Stdout under the CloudWatch agent, or a log stream via
// PutLogEvents — so CloudWatch extracts the bounded biz_* metric families
// (ADR-0004) while Logs Insights keeps the per-outcome amounts and ids.
//
// It is a nested module: a user who never touches CloudWatch does not pull
// the AWS SDK. The default path (EMF to a writer) makes no API calls at all;
// the optional PutMetricData path (WithMetricPutter) sends the metric
// families straight to the CloudWatch API for deployments that cannot ship
// logs. The two metric paths are mutually exclusive by construction — a
// putter replaces EMF metric records rather than adding to them, so metrics
// are never counted twice — while outcome events always take the writer.
//
// Each EMF record carries its own millisecond Timestamp from the point's or
// outcome's At, so — unlike the Prometheus exporter — a delayed batch keeps
// money pinned to observation time.
package cloudwatch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// unknownFamilyError names a metric family the exporter does not recognise —
// surfaced rather than silently dropped.
type unknownFamilyError struct{ name string }

func (e *unknownFamilyError) Error() string {
	return fmt.Sprintf("cloudwatch: unknown metric family %q", e.name)
}

// metricPutter is the slice of the CloudWatch API the optional direct path
// uses — an interface so a test (and LocalStack) substitutes a client.
type metricPutter interface {
	PutMetricData(ctx context.Context, in *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// ErrShutdown is returned by ExportMetrics and ExportEvents once Shutdown
// has run. Shutdown's flush is the last thing that moves the buffer, so a
// batch accepted after it would sit there forever — refused loudly instead
// (the emit.Exporter post-Shutdown contract).
var ErrShutdown = errors.New("cloudwatch: exporter is shut down")

// Exporter implements emit.Exporter over CloudWatch EMF.
type Exporter struct {
	namespace string
	unit      string

	mu     sync.Mutex // guards w and closed (bufio is not concurrency-safe)
	w      *bufio.Writer
	closed bool

	putter metricPutter // optional direct PutMetricData path
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	namespace string
	unit      string
	w         io.Writer
	putter    metricPutter
}

// WithWriter directs EMF records to w (default os.Stdout).
func WithWriter(w io.Writer) func(*Options) { return func(o *Options) { o.w = w } }

// WithNamespace overrides the CloudWatch namespace (default "shortfall").
func WithNamespace(ns string) func(*Options) { return func(o *Options) { o.namespace = ns } }

// WithUnit sets the EMF metric unit (default "None"). Amounts are minor
// currency units, which CloudWatch has no native unit for, so "None" is the
// honest default.
func WithUnit(u string) func(*Options) { return func(o *Options) { o.unit = u } }

// WithMetricPutter switches the metric path to the CloudWatch API: metric
// families are sent via PutMetricData instead of being written as EMF metric
// records — never both, which would double-count under agent extraction.
// Outcome events still go to the writer (PutMetricData cannot carry them).
// Pass a *cloudwatch.Client (or any PutMetricData implementer).
func WithMetricPutter(p metricPutter) func(*Options) { return func(o *Options) { o.putter = p } }

// New builds the exporter. With no options it writes EMF to os.Stdout in the
// "shortfall" namespace with unit "None".
func New(opts ...func(*Options)) *Exporter {
	o := Options{namespace: defaultNamespace, unit: "None", w: os.Stdout}
	for _, f := range opts {
		f(&o)
	}
	if o.namespace == "" {
		o.namespace = defaultNamespace
	}
	if o.unit == "" {
		o.unit = "None"
	}
	return &Exporter{
		namespace: o.namespace,
		unit:      o.unit,
		w:         bufio.NewWriter(o.w),
		putter:    o.putter,
	}
}

// Capabilities: EMF carries both signals — metric families as extracted
// metrics, outcomes as structured log records. Retention is CloudWatch's,
// unknown here, so the history weeks are 0.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: true, Events: true}
}

// ExportMetrics ships the metric families by exactly one path, never both:
// with a putter configured it calls PutMetricData; otherwise it writes EMF
// metric records for log-based extraction. Emitting both would double-count
// every metric under agent extraction. Outcome events always go to the writer
// (see ExportEvents). An unknown family surfaces as an error.
func (e *Exporter) ExportMetrics(ctx context.Context, batch []emit.MetricPoint) error {
	if len(batch) == 0 {
		return nil
	}
	if e.putter != nil {
		// After the empty-batch return, never before: an empty batch drops
		// nothing, so there is nothing to be loud about.
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return ErrShutdown
		}
		return e.putMetricData(ctx, batch)
	}
	return e.writeMetricRecords(batch)
}

func (e *Exporter) writeMetricRecords(batch []emit.MetricPoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrShutdown
	}
	for _, p := range batch {
		rec, err := buildMetricRecord(e.namespace, e.unit, p)
		if err != nil {
			return err
		}
		if err := e.writeLine(rec); err != nil {
			return err
		}
	}
	return nil
}

// putMetricData sends the batch to the CloudWatch API. Datums are capped at
// 1000 per call by the service, so the batch is chunked.
func (e *Exporter) putMetricData(ctx context.Context, batch []emit.MetricPoint) error {
	const maxDatums = 1000
	datums := make([]cwtypes.MetricDatum, 0, len(batch))
	for _, p := range batch {
		dims := dimsFor(p.Name)
		if dims == nil {
			return &unknownFamilyError{name: p.Name}
		}
		d := cwtypes.MetricDatum{
			MetricName: aws.String(p.Name),
			Value:      aws.Float64(float64(p.Value)),
			Timestamp:  aws.Time(p.At),
			Dimensions: make([]cwtypes.Dimension, 0, len(dims)),
		}
		for _, name := range dims {
			d.Dimensions = append(d.Dimensions, cwtypes.Dimension{
				Name:  aws.String(name),
				Value: aws.String(p.Labels[name]),
			})
		}
		datums = append(datums, d)
	}
	for start := 0; start < len(datums); start += maxDatums {
		end := start + maxDatums
		if end > len(datums) {
			end = len(datums)
		}
		if _, err := e.putter.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(e.namespace),
			MetricData: datums[start:end],
		}); err != nil {
			return fmt.Errorf("cloudwatch: put metric data: %w", err)
		}
	}
	return nil
}

// ExportEvents writes one structured EMF log record per outcome.
func (e *Exporter) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	if len(batch) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrShutdown
	}
	for _, o := range batch {
		rec, err := buildEventRecord(o)
		if err != nil {
			return err
		}
		if err := e.writeLine(rec); err != nil {
			return err
		}
	}
	return nil
}

// writeLine writes one record followed by a newline (EMF is line-delimited
// JSON). Callers hold e.mu.
func (e *Exporter) writeLine(rec []byte) error {
	if _, err := e.w.Write(rec); err != nil {
		return fmt.Errorf("cloudwatch: write: %w", err)
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("cloudwatch: write: %w", err)
	}
	return nil
}

// Shutdown flushes the buffered writer — records held in the buffer are
// outcome data that must reach the log stream, so a flush error surfaces —
// and is terminal: exports arriving from then on return ErrShutdown.
// Idempotent: the buffer a repeat call flushes is empty, so it returns nil.
func (e *Exporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	if err := e.w.Flush(); err != nil {
		return fmt.Errorf("cloudwatch: flush: %w", err)
	}
	return nil
}
