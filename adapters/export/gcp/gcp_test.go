package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

func sampleOutcome() biz.Outcome {
	return biz.Outcome{
		At: testTime,
		VC: biz.ValueContext{
			Flow:       "invoice.pay",
			EntityID:   "inv_123",
			CustomerID: "h:acct_9",
			Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
			Kind:       biz.KindFee,
			Segment:    "smb",
		},
		Stage:  "capture",
		Result: biz.ResultFailed,
	}
}

func TestCapabilitiesAreHonest(t *testing.T) {
	cases := []struct {
		name        string
		opts        []func(*Options)
		wantMetrics bool
		wantEvents  bool
	}{
		{
			name:        "no monitoring client means events only",
			opts:        []func(*Options){WithWriter(io.Discard)},
			wantMetrics: false,
			wantEvents:  true,
		},
		{
			name:        "monitoring client enables metrics",
			opts:        []func(*Options){WithWriter(io.Discard), WithMonitoring("p", &recordingDoer{})},
			wantMetrics: true,
			wantEvents:  true,
		},
		{
			name:        "a project without a doer does not enable metrics",
			opts:        []func(*Options){WithWriter(io.Discard), WithMonitoring("p", nil)},
			wantMetrics: false,
			wantEvents:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caps := New(c.opts...).Capabilities()
			if caps.Metrics != c.wantMetrics {
				t.Errorf("Metrics = %v, want %v", caps.Metrics, c.wantMetrics)
			}
			if caps.Events != c.wantEvents {
				t.Errorf("Events = %v, want %v", caps.Events, c.wantEvents)
			}
		})
	}
}

// TestMetricExportWithoutMonitoringIsNoOp pins the honest-incapable path: an
// exporter declaring Metrics false does not attempt the call, and does not
// report an error for a capability it never claimed.
func TestMetricExportWithoutMonitoringIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	e := New(WithWriter(&buf))
	pt := emit.MetricPoint{
		Name:   "biz_txn_total",
		Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
		Value:  1, At: testTime,
	}
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
		t.Fatalf("metric export on an events-only exporter: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("metric data reached the log writer: %q — metrics and events are separate paths", buf.String())
	}
}

func TestEventRecordCarriesTheMoneyFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*biz.Outcome)
		want    map[string]any
		absent  []string
		project string
	}{
		{
			name:   "a failed capture carries amount, ids, and outcome",
			mutate: func(*biz.Outcome) {},
			want: map[string]any{
				"event":            "biz.outcome",
				"severity":         "INFO",
				"biz.flow":         "invoice.pay",
				"biz.stage":        "capture",
				"biz.outcome":      "failed",
				"biz.entity.id":    "inv_123",
				"biz.customer.id":  "h:acct_9",
				"biz.amount_minor": float64(4999),
				"biz.currency":     "USD",
				"biz.exponent":     float64(2),
				"biz.value.kind":   "fee",
				"biz.segment":      "smb",
				"biz.amount.est":   false,
			},
			absent: []string{"source", "error", "trace.id"},
		},
		{
			name: "an empty segment is omitted rather than sent blank",
			mutate: func(o *biz.Outcome) {
				o.VC.Segment = ""
			},
			absent: []string{"biz.segment"},
		},
		{
			name: "source and error ride along when present",
			mutate: func(o *biz.Outcome) {
				o.Source = "stripe:webhook"
				o.Err = "card_declined"
			},
			want: map[string]any{"source": "stripe:webhook", "error": "card_declined"},
		},
		{
			name: "an estimated amount is marked",
			mutate: func(o *biz.Outcome) {
				o.VC.Estimated = true
			},
			want: map[string]any{"biz.amount.est": true},
		},
		{
			name: "a trace id correlates with Cloud Trace when a project is known",
			mutate: func(o *biz.Outcome) {
				o.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
			},
			project: "proj-1",
			want: map[string]any{
				"trace.id":                     "4bf92f3577b34da6a3ce929d0e0e4736",
				"logging.googleapis.com/trace": "projects/proj-1/traces/4bf92f3577b34da6a3ce929d0e0e4736",
			},
		},
		{
			name: "without a project the trace id stays a plain payload field",
			mutate: func(o *biz.Outcome) {
				o.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
			},
			want:   map[string]any{"trace.id": "4bf92f3577b34da6a3ce929d0e0e4736"},
			absent: []string{"logging.googleapis.com/trace"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := sampleOutcome()
			c.mutate(&o)
			rec, err := buildEventRecord(c.project, o)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(rec, &got); err != nil {
				t.Fatalf("record is not valid JSON: %v", err)
			}
			for k, want := range c.want {
				if got[k] != want {
					t.Errorf("%s = %#v, want %#v", k, got[k], want)
				}
			}
			for _, k := range c.absent {
				if _, present := got[k]; present {
					t.Errorf("%s should be absent, got %#v", k, got[k])
				}
			}
		})
	}
}

// TestEventRecordTimeIsObservationTime pins that the entry is stamped with
// the outcome's own time, not the time it was written: webhook deliveries
// arrive late during exactly the outages this library measures, and
// receipt-time stamping would move realized loss across incident windows.
func TestEventRecordTimeIsObservationTime(t *testing.T) {
	o := sampleOutcome()
	o.At = time.Date(2026, 8, 28, 14, 30, 15, 500000000, time.UTC)
	rec, err := buildEventRecord("", o)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rec, &got); err != nil {
		t.Fatal(err)
	}
	const want = "2026-08-28T14:30:15.5Z"
	if got["time"] != want {
		t.Errorf("time = %v, want %v", got["time"], want)
	}
}

// TestEventsAreLineDelimitedAndFlushOnShutdown pins the two properties Cloud
// Logging's stdout path depends on: one JSON object per line, and nothing
// left in the buffer once the exporter is shut down.
func TestEventsAreLineDelimitedAndFlushOnShutdown(t *testing.T) {
	cases := []struct {
		name  string
		count int
	}{
		{"single event", 1},
		{"several events", 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := New(WithWriter(&buf))
			batch := make([]biz.Outcome, 0, c.count)
			for range c.count {
				batch = append(batch, sampleOutcome())
			}
			if err := e.ExportEvents(context.Background(), batch); err != nil {
				t.Fatalf("export: %v", err)
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
			if len(lines) != c.count {
				t.Fatalf("got %d lines, want %d", len(lines), c.count)
			}
			for i, line := range lines {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Errorf("line %d is not a JSON object: %v", i, err)
				}
			}
		})
	}
}

// TestEmptyBatchesAreNoOps pins that nothing is written for an empty batch —
// a stray blank line would break the one-object-per-line contract.
func TestEmptyBatchesAreNoOps(t *testing.T) {
	var buf bytes.Buffer
	d := &recordingDoer{}
	e := New(WithWriter(&buf), WithMonitoring("p", d))
	if err := e.ExportEvents(context.Background(), nil); err != nil {
		t.Fatalf("empty event batch: %v", err)
	}
	if err := e.ExportMetrics(context.Background(), nil); err != nil {
		t.Fatalf("empty metric batch: %v", err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q for empty batches, want nothing", buf.String())
	}
	if len(d.reqs) != 0 {
		t.Errorf("sent %d requests for an empty batch, want none", len(d.reqs))
	}
}

// TestShutdownSurfacesFlushErrors pins that buffered outcome data failing to
// reach the log is reported rather than swallowed.
func TestShutdownSurfacesFlushErrors(t *testing.T) {
	e := New(WithWriter(failingWriter{}))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{sampleOutcome()}); err != nil {
		// Buffered: the small record fits, so the error surfaces at flush.
		t.Fatalf("export: %v", err)
	}
	if err := e.Shutdown(context.Background()); err == nil {
		t.Fatal("want a flush error, got nil — dropped outcome data must be visible")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestMetricPrefixIsConfigurable covers the override and its empty-string
// fallback, and pins that the family's biz_ prefix is what gets replaced.
func TestMetricPrefixIsConfigurable(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   string
	}{
		{"default prefix", "", "custom.googleapis.com/biz/txn_total"},
		{"explicit default", "custom.googleapis.com/biz/", "custom.googleapis.com/biz/txn_total"},
		{"custom domain prefix", "custom.googleapis.com/acme/money/", "custom.googleapis.com/acme/money/txn_total"},
		{"external metric prefix", "external.googleapis.com/prometheus/", "external.googleapis.com/prometheus/txn_total"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			opts := []func(*Options){
				WithWriter(io.Discard),
				WithMonitoring("proj-1", d),
				WithMonitoringEndpoint("https://monitoring.example"),
			}
			if c.prefix != "" {
				opts = append(opts, WithMetricPrefix(c.prefix))
			}
			pt := emit.MetricPoint{
				Name:   "biz_txn_total",
				Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
				Value:  1, At: testTime,
			}
			if err := New(opts...).ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
				t.Fatalf("export: %v", err)
			}
			if got := d.allSeries()[0].Metric.Type; got != c.want {
				t.Errorf("metric type = %q, want %q", got, c.want)
			}
		})
	}
}

// TestCumulativeSeriesAreWriterScoped pins that the monitored resource
// distinguishes one writer from another. Cloud Monitoring keys a series by
// metric type, metric labels, and resource — and the ADR-0004 label sets carry
// no writer identity — so a resource shared across replicas would put N
// independent running totals, each with its own start time, on one series.
func TestCumulativeSeriesAreWriterScoped(t *testing.T) {
	d := &recordingDoer{}
	e, _ := newTestExporter(t, d)
	pt := emit.MetricPoint{
		Name:   "biz_txn_total",
		Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
		Value:  1, At: testTime,
	}
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
		t.Fatalf("export: %v", err)
	}
	res := d.allSeries()[0].Resource
	if res.Type == "global" {
		t.Error("resource type is global — it carries no writer identity, so replicas share one cumulative series")
	}
	taskID := res.Labels["task_id"]
	if taskID == "" {
		t.Fatal("resource carries no task_id — replicas would overwrite each other's running totals")
	}
	if !strings.Contains(taskID, strconv.Itoa(os.Getpid())) {
		t.Errorf("task_id = %q, want it to distinguish this process (pid %d)", taskID, os.Getpid())
	}
	if res.Labels["project_id"] != "proj-1" {
		t.Errorf("resource project_id = %q, want proj-1", res.Labels["project_id"])
	}
}

func TestWithResourceOverrides(t *testing.T) {
	cases := []struct {
		name    string
		resType string
		labels  map[string]string
	}{
		{
			name:    "k8s container",
			resType: "k8s_container",
			labels:  map[string]string{"project_id": "proj-1", "pod_name": "payments-7d9", "container_name": "app"},
		},
		{
			name:    "gce instance",
			resType: "gce_instance",
			labels:  map[string]string{"project_id": "proj-1", "instance_id": "1234567890"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e := New(
				WithWriter(io.Discard),
				WithMonitoring("proj-1", d),
				WithMonitoringEndpoint("https://monitoring.example"),
				WithResource(c.resType, c.labels),
			)
			pt := emit.MetricPoint{
				Name:   "biz_dropped_events_total",
				Labels: map[string]string{"reason": "export"},
				Value:  1, At: testTime,
			}
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
				t.Fatalf("export: %v", err)
			}
			res := d.allSeries()[0].Resource
			if res.Type != c.resType {
				t.Errorf("resource type = %q, want %q", res.Type, c.resType)
			}
			for k, want := range c.labels {
				if res.Labels[k] != want {
					t.Errorf("resource label %s = %q, want %q", k, res.Labels[k], want)
				}
			}
		})
	}
}

// TestWithResourceCopiesItsLabels pins that a caller mutating its map after
// construction cannot change what the exporter writes.
func TestWithResourceCopiesItsLabels(t *testing.T) {
	labels := map[string]string{"project_id": "proj-1", "instance_id": "one"}
	d := &recordingDoer{}
	e := New(
		WithWriter(io.Discard),
		WithMonitoring("proj-1", d),
		WithMonitoringEndpoint("https://monitoring.example"),
		WithResource("gce_instance", labels),
	)
	labels["instance_id"] = "mutated"
	pt := emit.MetricPoint{
		Name:   "biz_dropped_events_total",
		Labels: map[string]string{"reason": "export"},
		Value:  1, At: testTime,
	}
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if got := d.allSeries()[0].Resource.Labels["instance_id"]; got != "one" {
		t.Errorf("instance_id = %q, want one — the option must copy its labels", got)
	}
}
