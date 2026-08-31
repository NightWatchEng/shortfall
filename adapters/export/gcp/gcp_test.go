// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

var testTime = time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC)

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

// TestCapabilitiesAreHonest pins that the exporter declares what it does:
// Cloud Logging extracts no metrics from log entries, and this module ships
// no Cloud Monitoring client, so Metrics is false on every configuration.
// Google Cloud metrics ship through adapters/export/otlp.
func TestCapabilitiesAreHonest(t *testing.T) {
	cases := []struct {
		name string
		opts []func(*Options)
	}{
		{name: "default configuration", opts: nil},
		{name: "with a writer", opts: []func(*Options){WithWriter(io.Discard)}},
		{name: "with a project", opts: []func(*Options){WithWriter(io.Discard), WithProject("p")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caps := New(c.opts...).Capabilities()
			if caps.Metrics {
				t.Error("Metrics = true — this exporter has no metric path; OTLP is the GCP metrics path")
			}

			if !caps.Events {
				t.Error("Events = false, want true")
			}
		})
	}
}

// TestMetricExportIsAnHonestNoOp pins the honest-incapable path: an exporter
// declaring Metrics false delivers no metric data anywhere — in particular
// not down the one output channel it has, the log writer — and does not
// report an error for a capability it never claimed.
func TestMetricExportIsAnHonestNoOp(t *testing.T) {
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

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("metric data reached the log writer: %q — an events-only exporter ships no metrics", buf.String())
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
				"event":                "biz.outcome",
				"severity":             "INFO",
				"biz.flow":             "invoice.pay",
				"biz.stage":            "capture",
				"biz.outcome":          "failed",
				"biz.entity.id":        "inv_123",
				"biz.customer.id":      "h:acct_9",
				"biz.amount.minor":     float64(4999),
				"biz.amount.currency":  "USD",
				"biz.amount.exponent":  float64(2),
				"biz.value.kind":       "fee",
				"biz.segment":          "smb",
				"biz.amount.estimated": false,
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
			want: map[string]any{"biz.amount.estimated": true},
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
	e := New(WithWriter(&buf))
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

// TestWithProjectDrivesTraceCorrelation pins that the project reaches the
// record through the exporter, not only through buildEventRecord. WithProject
// is the events path's own configuration now that the metric options which
// used to carry the project id are gone — without it the Cloud Trace
// correlation key would be unreachable in production.
func TestWithProjectDrivesTraceCorrelation(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	cases := []struct {
		name string
		opts []func(*Options)
		want any // the correlation field's value, or nil when it must be absent
	}{
		{
			name: "a project produces the reserved correlation key",
			opts: []func(*Options){WithProject("proj-1")},
			want: "projects/proj-1/traces/" + traceID,
		},
		{
			name: "no project leaves the trace id a plain payload field",
			opts: nil,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := New(append([]func(*Options){WithWriter(&buf)}, c.opts...)...)
			o := sampleOutcome()
			o.TraceID = traceID
			if err := e.ExportEvents(context.Background(), []biz.Outcome{o}); err != nil {
				t.Fatalf("export: %v", err)
			}

			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatalf("shutdown: %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
				t.Fatalf("record is not valid JSON: %v", err)
			}

			if got["trace.id"] != traceID {
				t.Errorf("trace.id = %#v, want %q", got["trace.id"], traceID)
			}

			if c.want == nil {
				if v, present := got["logging.googleapis.com/trace"]; present {
					t.Errorf("correlation key present without a project: %#v", v)
				}

				return
			}

			if got["logging.googleapis.com/trace"] != c.want {
				t.Errorf("logging.googleapis.com/trace = %#v, want %#v", got["logging.googleapis.com/trace"], c.want)
			}
		})
	}
}

// TestPostShutdownExportRefused pins the terminal branch of emit.Exporter's
// post-Shutdown contract: a later ExportEvents returns ErrShutdown and
// writes nothing — the writer's flush has already happened, so accepting
// the batch would absorb it silently.
func TestPostShutdownExportRefused(t *testing.T) {
	var buf bytes.Buffer
	e := New(WithWriter(&buf))
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	flushed := buf.Len()
	if err := e.ExportEvents(context.Background(), []biz.Outcome{sampleOutcome()}); !errors.Is(err, ErrShutdown) {
		t.Fatalf("post-shutdown ExportEvents err = %v, want ErrShutdown", err)
	}

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	if buf.Len() != flushed {
		t.Fatalf("post-shutdown export reached the writer: %d bytes appeared", buf.Len()-flushed)
	}

	// Empty batches stay a no-op even after Shutdown: nothing to refuse.
	if err := e.ExportEvents(context.Background(), nil); err != nil {
		t.Fatalf("post-shutdown empty batch errored: %v", err)
	}
}
