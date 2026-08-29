package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/emit"
)

var testTime = time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC)

// recordingDoer captures every request body and replies with a fixed status.
type recordingDoer struct {
	status int
	body   string
	err    error
	reqs   []timeSeriesRequest
	raw    [][]byte
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	d.raw = append(d.raw, raw)
	var parsed timeSeriesRequest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	d.reqs = append(d.reqs, parsed)
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(d.body)),
		Header:     make(http.Header),
	}, nil
}

// allSeries flattens every series the doer received, in order.
func (d *recordingDoer) allSeries() []timeSeries {
	var out []timeSeries
	for _, r := range d.reqs {
		out = append(out, r.TimeSeries...)
	}
	return out
}

// newTestExporter wires an exporter to a recording doer.
func newTestExporter(t *testing.T, d *recordingDoer) (*Exporter, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	e := New(
		WithWriter(&buf),
		WithMonitoring("proj-1", d),
		WithMonitoringEndpoint("https://monitoring.example/"),
	)
	return e, &buf
}

func TestSeriesShapePerFamily(t *testing.T) {
	cases := []struct {
		name       string
		point      emit.MetricPoint
		wantType   string
		wantKind   string
		wantLabels []string
	}{
		{
			name: "value total is a cumulative counter with six labels",
			point: emit.MetricPoint{
				Name: "biz_value_total",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
					"currency": "USD", "kind": "fee", "segment": "smb",
				},
				Value: 4999, At: testTime,
			},
			wantType:   "custom.googleapis.com/biz/value_total",
			wantKind:   "CUMULATIVE",
			wantLabels: []string{"currency", "flow", "kind", "outcome", "segment", "stage"},
		},
		{
			name: "txn total is a cumulative counter with five labels",
			point: emit.MetricPoint{
				Name: "biz_txn_total",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
					"currency": "USD", "segment": "smb",
				},
				Value: 1, At: testTime,
			},
			wantType:   "custom.googleapis.com/biz/txn_total",
			wantKind:   "CUMULATIVE",
			wantLabels: []string{"currency", "flow", "outcome", "segment", "stage"},
		},
		{
			name: "inflight value is a gauge with four labels",
			point: emit.MetricPoint{
				Name: "biz_inflight_value",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": "capture",
					"age_bucket": "5m-30m", "currency": "USD",
				},
				Value: 120000, At: testTime,
			},
			wantType:   "custom.googleapis.com/biz/inflight_value",
			wantKind:   "GAUGE",
			wantLabels: []string{"age_bucket", "currency", "flow", "stage"},
		},
		{
			name: "inflight count is a gauge",
			point: emit.MetricPoint{
				Name: "biz_inflight_count",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": "capture",
					"age_bucket": "5m-30m", "currency": "USD",
				},
				Value: 12, At: testTime,
			},
			wantType:   "custom.googleapis.com/biz/inflight_count",
			wantKind:   "GAUGE",
			wantLabels: []string{"age_bucket", "currency", "flow", "stage"},
		},
		{
			name: "provider calls is a cumulative counter with three labels",
			point: emit.MetricPoint{
				Name:   "biz_provider_calls_total",
				Labels: map[string]string{"provider": "stripe", "op": "capture", "outcome": "failed"},
				Value:  1, At: testTime,
			},
			wantType:   "custom.googleapis.com/biz/provider_calls_total",
			wantKind:   "CUMULATIVE",
			wantLabels: []string{"op", "outcome", "provider"},
		},
		{
			name: "dropped events is a cumulative counter with one label",
			point: emit.MetricPoint{
				Name:   "biz_dropped_events_total",
				Labels: map[string]string{"reason": "overflow"},
				Value:  3, At: testTime,
			},
			wantType:   "custom.googleapis.com/biz/dropped_events_total",
			wantKind:   "CUMULATIVE",
			wantLabels: []string{"reason"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{c.point}); err != nil {
				t.Fatalf("export: %v", err)
			}
			series := d.allSeries()
			if len(series) != 1 {
				t.Fatalf("got %d series, want 1", len(series))
			}
			s := series[0]
			if s.Metric.Type != c.wantType {
				t.Errorf("metric type = %q, want %q", s.Metric.Type, c.wantType)
			}
			if s.MetricKind != c.wantKind {
				t.Errorf("metric kind = %q, want %q", s.MetricKind, c.wantKind)
			}
			if s.ValueType != "INT64" {
				t.Errorf("value type = %q, want INT64 — money is never a double", s.ValueType)
			}
			if got := sortedKeys(s.Metric.Labels); !equalStrings(got, c.wantLabels) {
				t.Errorf("labels = %v, want exactly %v (ADR-0004 fixes the set)", got, c.wantLabels)
			}
			if s.Resource.Labels["project_id"] != "proj-1" {
				t.Errorf("resource project_id = %q, want proj-1", s.Resource.Labels["project_id"])
			}
		})
	}
}

// TestAmountsNeverSerialiseAsFloat pins the representation money leaves the
// process in: proto3 encodes int64 as a JSON string, and a bare JSON number
// would be read back as a double, silently rounding above 2^53 minor units.
func TestAmountsNeverSerialiseAsFloat(t *testing.T) {
	cases := []struct {
		name  string
		value int64
	}{
		{"small amount", 4999},
		{"beyond exact float64 integers", 1 << 54},
		{"max int64", 1<<63 - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			pt := emit.MetricPoint{
				Name: "biz_value_total",
				Labels: map[string]string{
					"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
					"currency": "USD", "kind": "fee", "segment": "smb",
				},
				Value: c.value, At: testTime,
			}
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
				t.Fatalf("export: %v", err)
			}
			want := fmt.Sprintf("%d", c.value)
			if got := d.allSeries()[0].Points[0].Value.Int64Value; got != want {
				t.Errorf("int64Value = %q, want %q", got, want)
			}
			wire := string(d.raw[0])
			if !strings.Contains(wire, `"int64Value":"`+want+`"`) {
				t.Errorf("wire form did not carry the amount as a quoted int64: %s", wire)
			}
			if strings.Contains(wire, `"doubleValue"`) {
				t.Error("wire form carried a doubleValue — money must never leave as a float")
			}
		})
	}
}

func TestCumulativeCountersAccumulate(t *testing.T) {
	cases := []struct {
		name   string
		deltas []int64
		want   []string
	}{
		{"single delta", []int64{5}, []string{"5"}},
		{"deltas accumulate into a running total", []int64{5, 3, 2}, []string{"5", "8", "10"}},
		{"a zero delta republishes the same total", []int64{7, 0}, []string{"7", "7"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			for i, delta := range c.deltas {
				pt := emit.MetricPoint{
					Name:   "biz_txn_total",
					Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
					Value:  delta,
					At:     testTime.Add(time.Duration(i) * time.Second),
				}
				if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
					t.Fatalf("export %d: %v", i, err)
				}
			}
			var got []string
			for _, s := range d.allSeries() {
				got = append(got, s.Points[0].Value.Int64Value)
			}
			if !equalStrings(got, c.want) {
				t.Errorf("totals = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCumulativeIntervalIsNonEmpty pins that a cumulative point's interval
// always ends after it starts; the service rejects one that does not, which
// a point observed at or before process start would otherwise produce.
func TestCumulativeIntervalIsNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
	}{
		{"observation after start", time.Now().UTC().Add(time.Hour)},
		{"observation before start", time.Now().UTC().Add(-time.Hour)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			pt := emit.MetricPoint{
				Name:   "biz_dropped_events_total",
				Labels: map[string]string{"reason": "export"},
				Value:  1, At: c.at,
			}
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
				t.Fatalf("export: %v", err)
			}
			iv := d.allSeries()[0].Points[0].Interval
			start, err := time.Parse(time.RFC3339Nano, iv.StartTime)
			if err != nil {
				t.Fatalf("start time %q: %v", iv.StartTime, err)
			}
			end, err := time.Parse(time.RFC3339Nano, iv.EndTime)
			if err != nil {
				t.Fatalf("end time %q: %v", iv.EndTime, err)
			}
			if !end.After(start) {
				t.Errorf("interval %s..%s is empty — the service rejects it", iv.StartTime, iv.EndTime)
			}
		})
	}
}

// TestGaugeStaleSampleDoesNotOverwrite pins emit's order-by-At contract:
// overlapping flushes may deliver an older level after a newer one, and the
// older one must not be published on top of it.
func TestGaugeStaleSampleDoesNotOverwrite(t *testing.T) {
	cases := []struct {
		name    string
		offsets []time.Duration
		values  []int64
		want    []string
	}{
		{"increasing times all publish", []time.Duration{0, time.Second}, []int64{10, 20}, []string{"10", "20"}},
		{"stale sample is skipped", []time.Duration{time.Second, 0}, []int64{20, 10}, []string{"20"}},
		{"equal times both publish", []time.Duration{0, 0}, []int64{10, 11}, []string{"10", "11"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			for i := range c.offsets {
				pt := emit.MetricPoint{
					Name:   "biz_inflight_value",
					Labels: map[string]string{"flow": "f", "stage": "s", "age_bucket": "5m-30m", "currency": "USD"},
					Value:  c.values[i],
					At:     testTime.Add(c.offsets[i]),
				}
				if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
					t.Fatalf("export %d: %v", i, err)
				}
			}
			var got []string
			for _, s := range d.allSeries() {
				got = append(got, s.Points[0].Value.Int64Value)
			}
			if !equalStrings(got, c.want) {
				t.Errorf("published levels = %v, want %v", got, c.want)
			}
		})
	}
}

// TestGaugeStaleGuardIsPerFamily pins that the two in-flight families share
// a label set but not a stale-guard key: an unqualified key would let one
// family's timestamp suppress the other's.
func TestGaugeStaleGuardIsPerFamily(t *testing.T) {
	d := &recordingDoer{}
	e, _ := newTestExporter(t, d)
	labels := map[string]string{"flow": "f", "stage": "s", "age_bucket": "5m-30m", "currency": "USD"}
	batch := []emit.MetricPoint{
		{Name: "biz_inflight_value", Labels: labels, Value: 5000, At: testTime.Add(time.Second)},
		{Name: "biz_inflight_count", Labels: labels, Value: 3, At: testTime},
	}
	if err := e.ExportMetrics(context.Background(), batch); err != nil {
		t.Fatalf("export: %v", err)
	}
	if got := len(d.allSeries()); got != 2 {
		t.Errorf("published %d series, want 2 — the count family was suppressed by the value family's timestamp", got)
	}
}

func TestMetricExportRejections(t *testing.T) {
	cases := []struct {
		name    string
		point   emit.MetricPoint
		wantErr string
	}{
		{
			name:    "unknown family surfaces rather than being dropped",
			point:   emit.MetricPoint{Name: "biz_made_up_total", Labels: map[string]string{}, Value: 1, At: testTime},
			wantErr: `unknown metric family "biz_made_up_total"`,
		},
		{
			name: "negative counter delta is rejected",
			point: emit.MetricPoint{
				Name:   "biz_txn_total",
				Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
				Value:  -1, At: testTime,
			},
			wantErr: "negative delta",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			err := e.ExportMetrics(context.Background(), []emit.MetricPoint{c.point})
			if err == nil {
				t.Fatal("want an error, got nil — a rejected point must never be silently dropped")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, c.wantErr)
			}
			if len(d.reqs) != 0 {
				t.Errorf("a rejected batch reached the backend (%d requests)", len(d.reqs))
			}
		})
	}
}

func TestTransportFailuresSurface(t *testing.T) {
	cases := []struct {
		name    string
		doer    *recordingDoer
		wantErr string
	}{
		{
			name:    "non-2xx surfaces with the status",
			doer:    &recordingDoer{status: http.StatusForbidden, body: "permission denied on metric"},
			wantErr: "403",
		},
		{
			name:    "non-2xx quotes the response body",
			doer:    &recordingDoer{status: http.StatusBadRequest, body: "startTime must precede endTime"},
			wantErr: "startTime must precede endTime",
		},
		{
			name:    "transport error surfaces",
			doer:    &recordingDoer{err: errors.New("dial tcp: connection refused")},
			wantErr: "connection refused",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newTestExporter(t, c.doer)
			pt := emit.MetricPoint{
				Name:   "biz_dropped_events_total",
				Labels: map[string]string{"reason": "export"},
				Value:  1, At: testTime,
			}
			err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt})
			if err == nil {
				t.Fatal("want an error, got nil — a failed export must not report success")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, c.wantErr)
			}
		})
	}
}

// TestBatchesChunkToServiceLimit pins that a batch larger than the service's
// per-request series cap is split rather than rejected wholesale.
func TestBatchesChunkToServiceLimit(t *testing.T) {
	cases := []struct {
		name      string
		points    int
		wantReqs  int
		wantFirst int
	}{
		{"under the limit is one request", 10, 1, 10},
		{"exactly the limit is one request", maxSeriesPerRequest, 1, maxSeriesPerRequest},
		{"over the limit splits", maxSeriesPerRequest + 1, 2, maxSeriesPerRequest},
		{"well over the limit splits evenly", maxSeriesPerRequest * 2, 2, maxSeriesPerRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			batch := make([]emit.MetricPoint, 0, c.points)
			for i := range c.points {
				batch = append(batch, emit.MetricPoint{
					Name:   "biz_dropped_events_total",
					Labels: map[string]string{"reason": fmt.Sprintf("r%d", i)},
					Value:  1, At: testTime,
				})
			}
			if err := e.ExportMetrics(context.Background(), batch); err != nil {
				t.Fatalf("export: %v", err)
			}
			if len(d.reqs) != c.wantReqs {
				t.Errorf("sent %d requests, want %d", len(d.reqs), c.wantReqs)
			}
			if got := len(d.reqs[0].TimeSeries); got != c.wantFirst {
				t.Errorf("first request carried %d series, want %d", got, c.wantFirst)
			}
			if got := len(d.allSeries()); got != c.points {
				t.Errorf("delivered %d series in total, want %d — chunking lost points", got, c.points)
			}
		})
	}
}

// TestRequestTargetsProjectTimeSeries pins the URL and method the adapter
// calls, which is the one thing a stubbed doer cannot otherwise catch.
func TestRequestTargetsProjectTimeSeries(t *testing.T) {
	var gotURL, gotMethod, gotContentType string
	d := &urlCapturingDoer{
		onRequest: func(r *http.Request) {
			gotURL = r.URL.String()
			gotMethod = r.Method
			gotContentType = r.Header.Get("Content-Type")
		},
	}
	e := New(WithWriter(io.Discard), WithMonitoring("proj-1", d), WithMonitoringEndpoint("https://monitoring.example/"))
	pt := emit.MetricPoint{
		Name:   "biz_dropped_events_total",
		Labels: map[string]string{"reason": "export"},
		Value:  1, At: testTime,
	}
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
		t.Fatalf("export: %v", err)
	}
	const wantURL = "https://monitoring.example/v3/projects/proj-1/timeSeries"
	if gotURL != wantURL {
		t.Errorf("URL = %q, want %q", gotURL, wantURL)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
}

type urlCapturingDoer struct{ onRequest func(*http.Request) }

func (d *urlCapturingDoer) Do(req *http.Request) (*http.Response, error) {
	d.onRequest(req)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
