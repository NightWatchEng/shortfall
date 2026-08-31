// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// syncBuffer is a concurrency-safe io.Writer: the exporter serialises its own
// bufio writer, but the harness reads the buffer too.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// gcpBackend reads what reached Cloud Logging out of the buffer the exporter
// flushed into, so it is consulted after Shutdown.
//
// This exporter declares Metrics=false, so the suite runs the capability
// honesty invariant against it rather than the no-loss one. MetricPoints is
// therefore a real observation, not a constant: the log writer is this
// exporter's ONLY output channel, so anything a metric path leaked would have
// to appear there. Counting the lines that are not outcome events is exactly
// the measurement that would catch such a leak — a hardcoded 0 would make the
// invariant unfalsifiable.
type gcpBackend struct{ sb *syncBuffer }

func (b gcpBackend) MetricPoints() int { return b.count(false) }

func (b gcpBackend) Events() int { return b.count(true) }

// count tallies written lines, either the outcome events or everything else.
// A line that is not valid JSON returns -1 to force a conformance failure
// rather than hide malformed output.
func (b gcpBackend) count(events bool) int {
	data := b.sb.snapshot()
	n := 0
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return -1
		}

		if (m["event"] == eventMarker) == events {
			n++
		}
	}

	return n
}

// gcpHarness runs the suite against the only configuration this exporter has:
// events to a writer, no metrics. The honest-incapable path is the path every
// GCP user runs.
type gcpHarness struct{}

func (gcpHarness) New() (emit.Exporter, conformance.Backend) {
	sb := &syncBuffer{}
	return New(WithWriter(sb), WithProject("proj-conformance")), gcpBackend{sb: sb}
}

func TestGCPConformance(t *testing.T) {
	conformance.RunExporter(t, gcpHarness{})
}
