package loki

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

// countingDoer tallies log lines across all posted Loki push bodies.
type countingDoer struct {
	mu     sync.Mutex
	events int
}

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	var pr pushRequest
	if err := json.Unmarshal(b, &pr); err == nil {
		for _, s := range pr.Streams {
			d.events += len(s.Values)
		}
	}
	return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
}

type backend struct{ d *countingDoer }

func (b backend) MetricPoints() int { return 0 } // Loki delivers no metrics
func (b backend) Events() int       { b.d.mu.Lock(); defer b.d.mu.Unlock(); return b.d.events }

type harness struct{}

func (harness) New() (emit.Exporter, conformance.Backend) {
	d := &countingDoer{}
	return New("https://loki/loki/api/v1/push", WithHTTPClient(d)), backend{d: d}
}

func TestLokiConformance(t *testing.T) {
	conformance.RunExporter(t, harness{})
}
