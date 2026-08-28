package datadog

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

var at = time.Date(2026, 8, 27, 20, 0, 0, 0, time.UTC)

// routeDoer records bodies keyed by whether the URL is the series or logs
// intake, and echoes the API key seen.
type routeDoer struct {
	series [][]byte
	logs   [][]byte
	apiKey string
	status int
}

func (d *routeDoer) Do(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	d.apiKey = req.Header.Get("DD-API-KEY")
	if strings.Contains(req.URL.Path, "/series") {
		d.series = append(d.series, b)
	} else if strings.Contains(req.URL.Path, "/logs") {
		d.logs = append(d.logs, b)
	}
	st := d.status
	if st == 0 {
		st = 202
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

func TestCapabilitiesBothSignals(t *testing.T) {
	e := New("key", WithHTTPClient(&routeDoer{}))
	if c := e.Capabilities(); !c.Metrics || !c.Events {
		t.Fatalf("caps = %+v", c)
	}
}

func TestMetricTypeAndTags(t *testing.T) {
	cases := []struct {
		name     string
		point    emit.MetricPoint
		wantType string
		wantTag  string
	}{
		{"counter is count type", emit.MetricPoint{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at}, "count", "currency:USD"},
		{"inflight is gauge type", emit.MetricPoint{Name: "biz_inflight_value", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}, Value: 500, At: at}, "gauge", "age_bucket:5m-30m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &routeDoer{}
			e := New("key", WithHTTPClient(d))
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{c.point}); err != nil {
				t.Fatal(err)
			}
			var p seriesPayload
			if err := json.Unmarshal(d.series[0], &p); err != nil {
				t.Fatal(err)
			}
			if p.Series[0].Type != c.wantType {
				t.Fatalf("type = %q, want %q", p.Series[0].Type, c.wantType)
			}
			if p.Series[0].Points[0][0] != float64(c.point.At.Unix()) {
				t.Fatalf("timestamp not preserved: %v", p.Series[0].Points[0][0])
			}
			found := false
			for _, tag := range p.Series[0].Tags {
				if tag == c.wantTag {
					found = true
				}
				// No amount/id may appear as a tag (ADR-0004).
				if strings.HasPrefix(tag, "amount") || strings.HasPrefix(tag, "entity") || strings.HasPrefix(tag, "customer") {
					t.Fatalf("forbidden tag %q", tag)
				}
			}
			if !found {
				t.Fatalf("missing tag %q in %v", c.wantTag, p.Series[0].Tags)
			}
		})
	}
}

func TestUnknownFamilyErrors(t *testing.T) {
	e := New("key", WithHTTPClient(&routeDoer{}))
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{{Name: "biz_bogus", Labels: map[string]string{}, Value: 1, At: at}}); err == nil {
		t.Fatal("unknown family must error")
	}
}

func TestApiKeyHeaderAndRouting(t *testing.T) {
	d := &routeDoer{}
	e := New("secret-key", WithHTTPClient(d))
	ctx := context.Background()
	if err := e.ExportMetrics(ctx, []emit.MetricPoint{{Name: "biz_txn_total", Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "o", "currency": "USD", "segment": "smb"}, Value: 1, At: at}}); err != nil {
		t.Fatal(err)
	}
	if err := e.ExportEvents(ctx, []biz.Outcome{{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed}}); err != nil {
		t.Fatal(err)
	}
	if d.apiKey != "secret-key" {
		t.Fatalf("DD-API-KEY = %q", d.apiKey)
	}
	if len(d.series) != 1 || len(d.logs) != 1 {
		t.Fatalf("routing wrong: series=%d logs=%d", len(d.series), len(d.logs))
	}
}

func TestEventTagsAreBoundedAmountsInMessage(t *testing.T) {
	d := &routeDoer{}
	e := New("key", WithHTTPClient(d))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed, Source: "stripe:webhook"}}); err != nil {
		t.Fatal(err)
	}
	var logs []logItem
	if err := json.Unmarshal(d.logs[0], &logs); err != nil {
		t.Fatal(err)
	}
	// ddtags carry only bounded dims — no amount/id.
	if strings.Contains(logs[0].DDTags, "amount") || strings.Contains(logs[0].DDTags, "inv_1") || strings.Contains(logs[0].DDTags, "h:c") {
		t.Fatalf("ddtags leaked unbounded data: %q", logs[0].DDTags)
	}
	// The amount and ids live in the message.
	for _, want := range []string{"biz.amount_minor", "inv_1", "h:c"} {
		if !strings.Contains(logs[0].Message, want) {
			t.Fatalf("message missing %q: %s", want, logs[0].Message)
		}
	}
}

func TestRetryPropagates(t *testing.T) {
	d := &routeDoer{status: 400}
	e := New("key", WithHTTPClient(d), WithBatcherOptions(httpbatch.WithRetry(2, time.Millisecond)))
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{{Name: "biz_txn_total", Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "o", "currency": "USD", "segment": "smb"}, Value: 1, At: at}}); err == nil {
		t.Fatal("permanent 400 must surface")
	}
	if len(d.series) != 1 {
		t.Fatalf("permanent 400 must not retry, series posts = %d", len(d.series))
	}
}

func TestGoldenPayloads(t *testing.T) {
	d := &routeDoer{}
	e := New("key", WithHTTPClient(d))
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

	var out bytes.Buffer
	out.WriteString("=== series ===\n")
	out.Write(indent(t, d.series[0]))
	out.WriteString("\n=== logs ===\n")
	out.Write(indent(t, d.logs[0]))
	out.WriteByte('\n')
	got := out.Bytes()

	golden := filepath.Join("testdata", "datadog.golden")
	if *update {
		must(os.WriteFile(golden, got, 0o644))
	}
	want, err := os.ReadFile(golden)
	must(err)
	if !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func indent(t *testing.T, b []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}
