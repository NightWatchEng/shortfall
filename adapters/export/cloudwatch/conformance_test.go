// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package cloudwatch

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// cwBackend counts EMF records by kind: a metric record declares
// CloudWatchMetrics, an event record carries event=biz.outcome. It reads the
// buffer the exporter flushed into, so it must be consulted after Shutdown
// (which the conformance suite does).
type cwBackend struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (b cwBackend) counts() (metrics, events int) {
	b.mu.Lock()
	data := append([]byte(nil), b.buf.Bytes()...)
	b.mu.Unlock()
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return -1, -1 // force a conformance failure on malformed output
		}

		if aws, ok := m["_aws"].(map[string]any); ok {
			if _, isMetric := aws["CloudWatchMetrics"]; isMetric {
				metrics++
				continue
			}
		}

		if m["event"] == "biz.outcome" {
			events++
		}
	}

	return metrics, events
}

func (b cwBackend) MetricPoints() int { m, _ := b.counts(); return m }
func (b cwBackend) Events() int       { _, e := b.counts(); return e }

// syncBuffer is a concurrency-safe io.Writer: overlapping flushes (emit's
// contract) can call ExportMetrics on separate goroutines, and the exporter
// serialises its bufio writer, but the harness also reads the buffer, so the
// buffer itself is guarded too.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

type cwHarness struct{}

func (cwHarness) New() (emit.Exporter, conformance.Backend) {
	sb := &syncBuffer{}
	e := New(WithWriter(sb))
	return e, cwBackend{buf: &sb.buf, mu: &sb.mu}
}

func TestCloudWatchConformance(t *testing.T) {
	conformance.RunExporter(t, cwHarness{})
}
