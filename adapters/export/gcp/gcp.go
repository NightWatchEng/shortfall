// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package gcp exports shortfall's outcome events to Google Cloud Logging as
// structured log entries — one entry per outcome, carrying the exact amount
// and the entity/customer ids.
//
// It is events-only by construction. Metrics for Google Cloud ship over
// adapters/export/otlp: an OpenTelemetry Collector with the Google Cloud
// exporter writes the same biz_* families to Cloud Monitoring, and the delta
// temporality emit already produces needs no in-process accumulator to get
// there. This module carried a bespoke timeSeries.create client until
// 2026-08-29; it was removed because it reimplemented what the OpenTelemetry
// metric SDK owns, and two ways to ship one signal is two things to keep
// correct.
//
// The event path writes line-delimited JSON to an io.Writer — os.Stdout on
// Cloud Run, GKE, or anywhere the logging agent collects stdout — which
// Cloud Logging parses into a structured jsonPayload with no API call and no
// credentials at all. That is GCP's own recommended shape for structured
// logs, and it is what ADR-0002's slog fallback describes: any log pipeline
// that can ship JSON lines becomes an event sink.
//
// The module depends on nothing beyond the standard library and shortfall's
// core.
package gcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// ErrShutdown is returned by ExportEvents once Shutdown has run. Shutdown's
// flush is the last thing that moves the buffer, so a batch accepted after
// it would sit there forever — refused loudly instead (the emit.Exporter
// post-Shutdown contract).
var ErrShutdown = errors.New("gcp: exporter is shut down")

// Exporter implements emit.Exporter over Cloud Logging.
type Exporter struct {
	mu     sync.Mutex // guards w and closed (bufio is not concurrency-safe)
	w      *bufio.Writer
	closed bool

	projectID string
}

var _ emit.Exporter = (*Exporter)(nil)

// Options configures the exporter.
type Options struct {
	w         io.Writer
	projectID string
}

// WithWriter directs Cloud Logging entries to w (default os.Stdout).
func WithWriter(w io.Writer) func(*Options) { return func(o *Options) { o.w = w } }

// WithProject names the GCP project so an outcome carrying a trace id also
// carries logging.googleapis.com/trace, the reserved key Cloud Logging
// correlates with Cloud Trace. Without it the trace id still rides the
// payload as a plain field; only the correlation link is omitted, because
// the link's format embeds the project.
func WithProject(projectID string) func(*Options) {
	return func(o *Options) { o.projectID = projectID }
}

// New builds the exporter. With no options it writes Cloud Logging entries
// to os.Stdout.
func New(opts ...func(*Options)) *Exporter {
	o := Options{w: os.Stdout}
	for _, f := range opts {
		f(&o)
	}
	if o.w == nil {
		o.w = os.Stdout
	}
	return &Exporter{w: bufio.NewWriter(o.w), projectID: o.projectID}
}

// Capabilities reports events only. Cloud Logging extracts no metrics from
// log entries the way CloudWatch EMF does, so this exporter declares
// Metrics false rather than pretending to ship them; adapters/export/otlp is
// the metrics path for Google Cloud. Retention is the project's, unknown
// here, so the history weeks stay 0.
func (e *Exporter) Capabilities() emit.Caps {
	return emit.Caps{Metrics: false, Events: true}
}

// ExportMetrics is a no-op, matching the Metrics:false capability this
// exporter declares. It reports no error: emit may hand metric points to any
// exporter, and refusing a signal never claimed would turn an honest
// incapability into a flush failure.
func (e *Exporter) ExportMetrics(context.Context, []emit.MetricPoint) error {
	return nil
}

// ExportEvents writes one structured Cloud Logging entry per outcome.
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
// swallowed. It is terminal: exports arriving from then on return
// ErrShutdown. Idempotent: the buffer a repeat call flushes is empty, so
// it returns nil.
func (e *Exporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	if err := e.w.Flush(); err != nil {
		return fmt.Errorf("gcp: flush: %w", err)
	}
	return nil
}
