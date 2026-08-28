package statsd

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// syncBuffer is a concurrency-safe writer: overlapping flushes may call
// ExportMetrics on separate goroutines (emit's contract), and the harness
// also reads the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) lines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := strings.TrimSpace(s.buf.String())
	if t == "" {
		return 0
	}
	return strings.Count(t, "\n") + 1
}

type statsdBackend struct{ sb *syncBuffer }

func (b statsdBackend) MetricPoints() int { return b.sb.lines() }
func (b statsdBackend) Events() int       { return 0 }

type statsdHarness struct{}

func (statsdHarness) New() (emit.Exporter, conformance.Backend) {
	sb := &syncBuffer{}
	e, err := New(WithWriter(sb), WithLogger(slog.New(slog.NewTextHandler(&buf2{}, nil))))
	if err != nil {
		panic(err)
	}
	return e, statsdBackend{sb: sb}
}

func TestStatsDConformance(t *testing.T) {
	conformance.RunExporter(t, statsdHarness{})
}
