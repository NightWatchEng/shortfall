package splunkhec

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

// countingDoer tallies HEC records by kind from the posted bodies: an object
// with "event":"metric" is a metric, anything else is an event.
type countingDoer struct {
	mu      sync.Mutex
	metrics int
	events  int
}

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["event"] == "metric" {
			d.metrics++
		} else {
			d.events++
		}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
}

type backend struct{ d *countingDoer }

func (b backend) MetricPoints() int { b.d.mu.Lock(); defer b.d.mu.Unlock(); return b.d.metrics }
func (b backend) Events() int       { b.d.mu.Lock(); defer b.d.mu.Unlock(); return b.d.events }

type harness struct{}

func (harness) New() (emit.Exporter, conformance.Backend) {
	d := &countingDoer{}
	e := New("https://x/services/collector", "tok", WithHTTPClient(d))
	return e, backend{d: d}
}

func TestSplunkHECConformance(t *testing.T) {
	conformance.RunExporter(t, harness{})
}
