package datadog

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// countingDoer tallies series points and log entries across both intakes.
type countingDoer struct {
	mu      sync.Mutex
	metrics int
	events  int
}

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(req.URL.Path, "/series"):
		var p seriesPayload
		if err := json.Unmarshal(b, &p); err == nil {
			for _, s := range p.Series {
				d.metrics += len(s.Points)
			}
		}
	case strings.Contains(req.URL.Path, "/logs"):
		var logs []logItem
		if err := json.Unmarshal(b, &logs); err == nil {
			d.events += len(logs)
		}
	}
	return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(""))}, nil
}

type backend struct{ d *countingDoer }

func (b backend) MetricPoints() int { b.d.mu.Lock(); defer b.d.mu.Unlock(); return b.d.metrics }
func (b backend) Events() int       { b.d.mu.Lock(); defer b.d.mu.Unlock(); return b.d.events }

type harness struct{}

func (harness) New() (emit.Exporter, conformance.Backend) {
	d := &countingDoer{}
	return New("test-key", WithHTTPClient(d)), backend{d: d}
}

func TestDatadogConformance(t *testing.T) {
	conformance.RunExporter(t, harness{})
}
