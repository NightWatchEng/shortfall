package gcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// countingDoer tallies the time series Cloud Monitoring received. Overlapping
// flushes (emit's contract) can call ExportMetrics on separate goroutines, so
// the tally is guarded.
type countingDoer struct {
	mu     sync.Mutex
	series int
}

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var parsed timeSeriesRequest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("monitoring payload is not valid JSON: %w", err)
	}
	d.mu.Lock()
	d.series += len(parsed.TimeSeries)
	d.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

func (d *countingDoer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.series
}

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

// gcpBackend counts what reached each backend: time series posted to Cloud
// Monitoring, and outcome lines written for Cloud Logging. It reads the
// buffer the exporter flushed into, so it is consulted after Shutdown.
type gcpBackend struct {
	doer *countingDoer
	sb   *syncBuffer
}

func (b gcpBackend) MetricPoints() int { return b.doer.count() }

func (b gcpBackend) Events() int {
	b.sb.mu.Lock()
	data := append([]byte(nil), b.sb.buf.Bytes()...)
	b.sb.mu.Unlock()

	events := 0
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return -1 // force a conformance failure on malformed output
		}
		if m["event"] == eventMarker {
			events++
		}
	}
	return events
}

type gcpHarness struct{}

func (gcpHarness) New() (emit.Exporter, conformance.Backend) {
	sb := &syncBuffer{}
	d := &countingDoer{}
	e := New(
		WithWriter(sb),
		WithMonitoring("proj-conformance", d),
		WithMonitoringEndpoint("https://monitoring.example"),
	)
	return e, gcpBackend{doer: d, sb: sb}
}

func TestGCPConformance(t *testing.T) {
	conformance.RunExporter(t, gcpHarness{})
}

// eventsOnlyHarness exercises the suite against the default configuration —
// no monitoring client — so the capability-honesty invariant is checked on
// the path most GCP users will actually run.
type eventsOnlyHarness struct{}

func (eventsOnlyHarness) New() (emit.Exporter, conformance.Backend) {
	sb := &syncBuffer{}
	return New(WithWriter(sb)), gcpBackend{doer: &countingDoer{}, sb: sb}
}

func TestGCPEventsOnlyConformance(t *testing.T) {
	conformance.RunExporter(t, eventsOnlyHarness{})
}
