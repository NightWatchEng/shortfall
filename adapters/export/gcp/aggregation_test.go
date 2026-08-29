package gcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/emit"
)

// txnPoint builds a biz_txn_total point on one fixed series. Every field but
// the value and time is constant, so a batch of these is exactly the shape
// emit produces for many distinct entities in one flush interval.
func txnPoint(value int64, at time.Time) emit.MetricPoint {
	return emit.MetricPoint{
		Name: "biz_txn_total",
		Labels: map[string]string{
			"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
			"currency": "USD", "segment": "smb",
		},
		Value: value,
		At:    at,
	}
}

func inflightPoint(value int64, at time.Time) emit.MetricPoint {
	return emit.MetricPoint{
		Name: "biz_inflight_value",
		Labels: map[string]string{
			"flow": "invoice.pay", "stage": "capture",
			"age_bucket": "5m-30m", "currency": "USD",
		},
		Value: value,
		At:    at,
	}
}

// TestOneSeriesPerRequest pins the CreateTimeSeries contract: a request may
// carry at most one point per series, so points sharing a label set inside a
// batch are aggregated rather than sent as duplicate series. emit does no
// per-series coalescing — every Record on one flow/stage/outcome/currency
// appends another point — so an unaggregated batch would be rejected whole
// and the metric path would fail under any real traffic.
func TestOneSeriesPerRequest(t *testing.T) {
	cases := []struct {
		name   string
		batch  []emit.MetricPoint
		want   int
		wantV  []string
		reason string
	}{
		{
			name:  "counter points on one series collapse to one total",
			batch: []emit.MetricPoint{txnPoint(1, testTime), txnPoint(1, testTime), txnPoint(1, testTime)},
			want:  1,
			wantV: []string{"3"},
		},
		{
			name: "distinct series stay distinct",
			batch: []emit.MetricPoint{
				txnPoint(1, testTime),
				{
					Name: "biz_txn_total",
					Labels: map[string]string{
						"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
						"currency": "EUR", "segment": "smb",
					},
					Value: 1, At: testTime,
				},
			},
			want:  2,
			wantV: []string{"1", "1"},
		},
		{
			name:  "gauge points on one series collapse to the newest level",
			batch: []emit.MetricPoint{inflightPoint(10, testTime), inflightPoint(20, testTime.Add(time.Second))},
			want:  1,
			wantV: []string{"20"},
		},
		{
			name:  "an out-of-order gauge pair keeps the newest level",
			batch: []emit.MetricPoint{inflightPoint(20, testTime.Add(time.Second)), inflightPoint(10, testTime)},
			want:  1,
			wantV: []string{"20"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &recordingDoer{}
			e, _ := newTestExporter(t, d)
			if err := e.ExportMetrics(context.Background(), c.batch); err != nil {
				t.Fatalf("export: %v", err)
			}
			series := d.allSeries()
			if len(series) != c.want {
				t.Fatalf("sent %d series, want %d", len(series), c.want)
			}
			if err := assertNoDuplicateSeries(series); err != nil {
				t.Error(err)
			}
			var got []string
			for _, s := range series {
				got = append(got, s.Points[0].Value.Int64Value)
			}
			if !equalStrings(got, c.wantV) {
				t.Errorf("values = %v, want %v", got, c.wantV)
			}
		})
	}
}

// TestNoRequestEverCarriesDuplicateSeries is the general form: whatever the
// batch, no single request may contain two series with the same metric type,
// labels, and resource.
func TestNoRequestEverCarriesDuplicateSeries(t *testing.T) {
	d := &recordingDoer{}
	e, _ := newTestExporter(t, d)
	var batch []emit.MetricPoint
	for i := range 500 {
		batch = append(batch, txnPoint(1, testTime.Add(time.Duration(i)*time.Millisecond)))
		batch = append(batch, inflightPoint(int64(i), testTime.Add(time.Duration(i)*time.Millisecond)))
	}
	if err := e.ExportMetrics(context.Background(), batch); err != nil {
		t.Fatalf("export: %v", err)
	}
	for i, req := range d.reqs {
		if err := assertNoDuplicateSeries(req.TimeSeries); err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
	if got := len(d.allSeries()); got != 2 {
		t.Errorf("sent %d series for 2 distinct label sets, want 2", got)
	}
}

// assertNoDuplicateSeries reports a request that would be rejected whole by
// CreateTimeSeries for carrying one series twice.
func assertNoDuplicateSeries(series []timeSeries) error {
	seen := map[string]bool{}
	for _, s := range series {
		key := s.Metric.Type + "\x00" + s.Resource.Type
		for _, name := range sortedKeys(s.Metric.Labels) {
			key += "\x00" + name + "=" + s.Metric.Labels[name]
		}
		for _, name := range sortedKeys(s.Resource.Labels) {
			key += "\x00" + name + "=" + s.Resource.Labels[name]
		}
		if seen[key] {
			return errDuplicateSeries{typ: s.Metric.Type}
		}
		seen[key] = true
	}
	return nil
}

type errDuplicateSeries struct{ typ string }

func (e errDuplicateSeries) Error() string {
	return "request carries the same series twice (" + e.typ +
		") — CreateTimeSeries rejects the whole request"
}

// TestFailedSendCommitsNothing pins that accumulator state advances only for
// data that landed. emit re-credits failed biz_dropped_events_total deltas on
// the stated assumption that a failed export never left the process; banking
// a delta before the send would make the next published total count it twice.
func TestFailedSendCommitsNothing(t *testing.T) {
	cases := []struct {
		name   string
		family string
		labels map[string]string
		want   string
	}{
		{
			name:   "dropped events total is not inflated by a retry",
			family: "biz_dropped_events_total",
			labels: map[string]string{"reason": "export"},
			want:   "5",
		},
		{
			name:   "txn total is not inflated by a retry",
			family: "biz_txn_total",
			labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "USD", "segment": ""},
			want:   "5",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			failing := &recordingDoer{status: http.StatusServiceUnavailable, body: "backend unavailable"}
			e := New(
				WithWriter(discardWriter{}),
				WithMonitoring("proj-1", failing),
				WithMonitoringEndpoint("https://monitoring.example"),
			)
			pt := emit.MetricPoint{Name: c.family, Labels: c.labels, Value: 5, At: testTime}
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err == nil {
				t.Fatal("want an export error, got nil")
			}
			// emit re-credits the same delta and the caller retries it.
			failing.status = http.StatusOK
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{pt}); err != nil {
				t.Fatalf("retry: %v", err)
			}
			series := failing.allSeries()
			got := series[len(series)-1].Points[0].Value.Int64Value
			if got != c.want {
				t.Errorf("published total after a failed send and a retry = %q, want %q — the failed delta was banked twice", got, c.want)
			}
		})
	}
}

// TestFailedSendDoesNotAdvanceTheGaugeGuard pins the same rule for levels: a
// guard advanced by an undelivered sample would suppress the next older
// sample although nothing for that series was ever published.
func TestFailedSendDoesNotAdvanceTheGaugeGuard(t *testing.T) {
	failing := &recordingDoer{status: http.StatusServiceUnavailable, body: "backend unavailable"}
	e := New(
		WithWriter(discardWriter{}),
		WithMonitoring("proj-1", failing),
		WithMonitoringEndpoint("https://monitoring.example"),
	)
	newer := inflightPoint(20, testTime.Add(time.Second))
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{newer}); err == nil {
		t.Fatal("want an export error, got nil")
	}
	failing.status = http.StatusOK
	older := inflightPoint(10, testTime)
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{older}); err != nil {
		t.Fatalf("second export: %v", err)
	}
	series := failing.allSeries()
	if len(series) != 2 {
		t.Fatalf("published %d series, want 2 — the undelivered level suppressed the next sample", len(series))
	}
	if got := series[1].Points[0].Value.Int64Value; got != "10" {
		t.Errorf("published level = %q, want 10", got)
	}
}

// TestPartialChunkFailureDoesNotDoubleCountRecreditedDeltas pins the
// multi-chunk case. emit hands biz_dropped_events_total deltas back on ANY
// error from ExportMetrics, so a drop delta committed in a landed chunk of a
// batch that later fails would be counted twice on the next flush. A landed
// ordinary counter, by contrast, legitimately advances on the retry: its
// delta really was re-sent, and a cumulative series may not go backwards.
func TestPartialChunkFailureDoesNotDoubleCountRecreditedDeltas(t *testing.T) {
	d := &failOnNthDoer{failAt: 2}
	e := New(
		WithWriter(discardWriter{}),
		WithMonitoring("proj-1", d),
		WithMonitoringEndpoint("https://monitoring.example"),
	)
	// The drop counter lands in chunk one; chunk two fails.
	batch := []emit.MetricPoint{{
		Name:   "biz_dropped_events_total",
		Labels: map[string]string{"reason": "export"},
		Value:  5, At: testTime,
	}}
	for i := range maxSeriesPerRequest {
		batch = append(batch, emit.MetricPoint{
			Name:   "biz_txn_total",
			Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "failed", "currency": "c" + strconv.Itoa(i), "segment": ""},
			Value:  1, At: testTime,
		})
	}
	if err := e.ExportMetrics(context.Background(), batch); err == nil {
		t.Fatal("want an error from the failing second chunk, got nil")
	}
	if len(d.series) != maxSeriesPerRequest {
		t.Fatalf("chunk one delivered %d series, want %d", len(d.series), maxSeriesPerRequest)
	}
	if got := totalFor(d.series, "custom.googleapis.com/biz/dropped_events_total"); got != "5" {
		t.Fatalf("drop total on the first send = %q, want 5", got)
	}

	// emit re-credits the drop delta and the caller retries the whole batch.
	d.failAt = 0
	if err := e.ExportMetrics(context.Background(), batch); err != nil {
		t.Fatalf("retry: %v", err)
	}
	retried := d.series[maxSeriesPerRequest:]
	if got := totalFor(retried, "custom.googleapis.com/biz/dropped_events_total"); got != "5" {
		t.Errorf("drop total after a partial failure and a re-credited retry = %q, want 5 — the landed chunk banked it and emit credited it again", got)
	}
	if got := totalFor(retried, "custom.googleapis.com/biz/txn_total"); got != "2" {
		t.Errorf("txn total after the retry = %q, want 2 — a landed ordinary delta must stay committed so the cumulative series never goes backwards", got)
	}
}

// totalFor returns the first published value for a metric type, or "" when
// the type is absent.
func totalFor(series []timeSeries, metricType string) string {
	for _, s := range series {
		if s.Metric.Type == metricType {
			return s.Points[0].Value.Int64Value
		}
	}
	return ""
}

type failOnNthDoer struct {
	calls  int
	failAt int
	series []timeSeries
}

func (d *failOnNthDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	status := http.StatusOK
	if d.failAt != 0 && d.calls == d.failAt {
		status = http.StatusServiceUnavailable
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		var parsed timeSeriesRequest
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		d.series = append(d.series, parsed.TimeSeries...)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
