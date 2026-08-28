package loki

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

var at = time.Date(2026, 8, 27, 19, 0, 0, 0, time.UTC)

type captureDoer struct {
	bodies [][]byte
	orgID  string
	status int
}

func (d *captureDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.bodies = append(d.bodies, b)
	d.orgID = req.Header.Get("X-Scope-OrgID")
	st := d.status
	if st == 0 {
		st = 204
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func outcome(entity, result, stage string) biz.Outcome {
	return biz.Outcome{
		At: at, Stage: stage, Result: biz.Result(result),
		VC: biz.ValueContext{
			Flow: "invoice.pay", EntityID: entity, CustomerID: "h:c", Segment: "smb",
			Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
		},
	}
}

func TestCapabilitiesEventsOnly(t *testing.T) {
	e := New("https://loki/x", WithHTTPClient(&captureDoer{}))
	if c := e.Capabilities(); c.Metrics || !c.Events {
		t.Fatalf("caps = %+v, want events-only", c)
	}
}

func TestExportMetricsIsNoop(t *testing.T) {
	d := &captureDoer{}
	e := New("https://loki/x", WithHTTPClient(d))
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{{Name: "biz_value_total", Value: 1, At: at}}); err != nil {
		t.Fatal(err)
	}
	if len(d.bodies) != 0 {
		t.Fatalf("metrics must not be posted, got %d bodies", len(d.bodies))
	}
}

func TestAmountsAndIdsStayOutOfStreamLabels(t *testing.T) {
	d := &captureDoer{}
	e := New("https://loki/x", WithHTTPClient(d))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{outcome("inv_1", "failed", "capture")}); err != nil {
		t.Fatal(err)
	}
	var req pushRequest
	if err := json.Unmarshal(d.bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Streams) != 1 {
		t.Fatalf("streams = %d", len(req.Streams))
	}
	labels := req.Streams[0].Stream
	// Stream labels are only the bounded dims — no amount/id/customer.
	for k := range labels {
		if k != "flow" && k != "stage" && k != "outcome" {
			t.Fatalf("unexpected (unbounded?) stream label %q", k)
		}
	}
	// The amount and ids must be in the log line, not labels.
	line := req.Streams[0].Values[0][1]
	for _, want := range []string{"biz.amount_minor", "inv_1", "h:c"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %q: %s", want, line)
		}
	}
}

func TestOutcomesGroupedIntoStreams(t *testing.T) {
	d := &captureDoer{}
	e := New("https://loki/x", WithHTTPClient(d))
	// Two outcomes share (flow,stage,outcome); one differs by outcome.
	if err := e.ExportEvents(context.Background(), []biz.Outcome{
		outcome("inv_1", "failed", "capture"),
		outcome("inv_2", "failed", "capture"),
		outcome("inv_3", "success", "capture"),
	}); err != nil {
		t.Fatal(err)
	}
	var req pushRequest
	if err := json.Unmarshal(d.bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 (failed x2 grouped, success x1)", len(req.Streams))
	}
	total := 0
	for _, s := range req.Streams {
		total += len(s.Values)
	}
	if total != 3 {
		t.Fatalf("total values = %d, want 3", total)
	}
}

func TestOrgIDHeader(t *testing.T) {
	d := &captureDoer{}
	e := New("https://loki/x", WithHTTPClient(d), WithOrgID("tenant-7"))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{outcome("inv_1", "failed", "capture")}); err != nil {
		t.Fatal(err)
	}
	if d.orgID != "tenant-7" {
		t.Fatalf("X-Scope-OrgID = %q", d.orgID)
	}
}

func TestRetryPropagates(t *testing.T) {
	d := &captureDoer{status: 400}
	e := New("https://loki/x", WithHTTPClient(d), WithBatcherOptions(httpbatch.WithRetry(2, time.Millisecond)))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{outcome("inv_1", "failed", "capture")}); err == nil {
		t.Fatal("permanent 400 must surface")
	}
	if len(d.bodies) != 1 {
		t.Fatalf("permanent 400 must not retry, bodies = %d", len(d.bodies))
	}
}

// TestGoldenPush pins the exact Loki push body.
func TestGoldenPush(t *testing.T) {
	d := &captureDoer{}
	e := New("https://loki/x", WithHTTPClient(d))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{
		outcome("inv_1", "failed", "capture"),
		outcome("inv_3", "success", "capture"),
	}); err != nil {
		t.Fatal(err)
	}
	var m any
	if err := json.Unmarshal(d.bodies[0], &m); err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "push.golden")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("push body mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
