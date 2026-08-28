package splunkhec

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/adapters/httpbatch"
	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

var update = flag.Bool("update", false, "update golden files")

var at = time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)

// captureDoer records every request body and returns a fixed status.
type captureDoer struct {
	bodies [][]byte
	auth   string
	status int
}

func (d *captureDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.bodies = append(d.bodies, b)
	d.auth = req.Header.Get("Authorization")
	st := d.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func vc() biz.ValueContext {
	return biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_1", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
}
func valueLbls(cur string) map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": cur, "kind": "fee", "segment": "smb"}
}

func TestAuthHeaderAndUnknownFamily(t *testing.T) {
	d := &captureDoer{}
	e := New("https://splunk.example/services/collector", "tok123", WithHTTPClient(d))
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
	}); err != nil {
		t.Fatal(err)
	}
	if d.auth != "Splunk tok123" {
		t.Fatalf("auth header = %q", d.auth)
	}
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
		{Name: "biz_bogus", Labels: map[string]string{}, Value: 1, At: at},
	}); err == nil {
		t.Fatal("unknown family must error")
	}
}

func TestCapabilitiesBothSignals(t *testing.T) {
	e := New("https://x/y", "t")
	if c := e.Capabilities(); !c.Metrics || !c.Events {
		t.Fatalf("caps = %+v", c)
	}
}

func TestChunksAtMaxBatch(t *testing.T) {
	d := &captureDoer{}
	e := New("https://x/y", "t", WithHTTPClient(d))
	n := maxBatch*2 + 5
	batch := make([]emit.MetricPoint, n)
	for i := range batch {
		batch[i] = emit.MetricPoint{Name: "biz_txn_total", Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "o", "currency": "USD", "segment": "smb"}, Value: 1, At: at}
	}
	if err := e.ExportMetrics(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(d.bodies) != 3 { // 100 + 100 + 5
		t.Fatalf("posts = %d, want 3", len(d.bodies))
	}
	// Each body is newline-delimited JSON; total events == n.
	total := 0
	for _, b := range d.bodies {
		total += bytes.Count(b, []byte("\n")) + 1
	}
	if total != n {
		t.Fatalf("total events = %d, want %d", total, n)
	}
}

func TestRetryPropagatesFromBatcher(t *testing.T) {
	// A permanent 400 must surface as an error (no infinite loop).
	d := &captureDoer{status: 400}
	e := New("https://x/y", "t", WithHTTPClient(d),
		WithBatcherOptions(httpbatch.WithRetry(2, time.Millisecond)))
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
		{Name: "biz_txn_total", Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "o", "currency": "USD", "segment": "smb"}, Value: 1, At: at},
	}); err == nil {
		t.Fatal("permanent status must surface as error")
	}
	if len(d.bodies) != 1 {
		t.Fatalf("permanent 400 must not retry, posts = %d", len(d.bodies))
	}
}

// TestGoldenPayloads pins the exact HEC bodies for metrics and events.
func TestGoldenPayloads(t *testing.T) {
	d := &captureDoer{}
	e := New("https://x/y", "t", WithHTTPClient(d))
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.ExportMetrics(ctx, []emit.MetricPoint{
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
		{Name: "biz_inflight_value", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}, Value: 5568661, At: at},
	}))
	must(e.ExportEvents(ctx, []biz.Outcome{{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed, Source: "stripe:webhook"}}))

	got := normalize(t, d.bodies)
	golden := filepath.Join("testdata", "hec.golden")
	if *update {
		must(os.WriteFile(golden, got, 0o644))
	}
	want, err := os.ReadFile(golden)
	must(err)
	if !bytes.Equal(got, want) {
		t.Fatalf("HEC payload mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// normalize re-marshals every posted JSON object with indentation and sorted
// keys so the golden is stable regardless of map iteration order.
func normalize(t *testing.T, bodies [][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, body := range bodies {
		for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatalf("posted line is not JSON: %v\n%s", err, line)
			}
			enc, err := json.MarshalIndent(m, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			out.Write(enc)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}
