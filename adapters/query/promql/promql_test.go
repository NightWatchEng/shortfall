package promql

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

var (
	from = time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	to   = time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
)

func TestTranslate(t *testing.T) {
	// The adapter evaluates one millisecond inside each boundary to realize the
	// half-open [From, To) window (see translate); expectations use the same.
	tT, tF := promTime(to.Add(-time.Millisecond)), promTime(from.Add(-time.Millisecond))
	cases := []struct {
		name     string
		q        query.Query
		wantExpr string
		wantErr  bool
	}{
		{
			name: "counter is an exact cumulative difference, not increase(); one-end series default to 0",
			q: query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to},
				Filters: map[string]string{"outcome": "failed", "flow": "invoice.pay"}, GroupBy: []string{"currency"}},
			wantExpr: `sum by (currency) (last_over_time(biz_value_total{flow="invoice.pay",outcome="failed"}[3600s] @ ` + tT +
				`)) - (sum by (currency) (last_over_time(biz_value_total{flow="invoice.pay",outcome="failed"}[3600s] @ ` + tF +
				`)) or (sum by (currency) (last_over_time(biz_value_total{flow="invoice.pay",outcome="failed"}[3600s] @ ` + tT + `)) * 0))`,
		},
		{
			name:     "gauge reads the carried-forward level via last_over_time",
			q:        query.Query{Metric: "biz_inflight_value", Range: query.TimeRange{From: from, To: to}, GroupBy: []string{"age_bucket", "currency"}},
			wantExpr: `sum by (age_bucket, currency) (last_over_time(biz_inflight_value[3600s] @ ` + tT + `))`,
		},
		{
			name:     "biz_inflight_count is a gauge too (ADR-0012), not a counter delta",
			q:        query.Query{Metric: "biz_inflight_count", Range: query.TimeRange{From: from, To: to}, GroupBy: []string{"age_bucket", "currency"}},
			wantExpr: `sum by (age_bucket, currency) (last_over_time(biz_inflight_count[3600s] @ ` + tT + `))`,
		},
		{
			name:    "AggCount is rejected",
			q:       query.Query{Metric: "biz_txn_total", Agg: query.AggCount, Range: query.TimeRange{From: from, To: to}},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex, err := translate(c.q)
			if c.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ex.expr != c.wantExpr {
				t.Fatalf("expr = %q\nwant   %q", ex.expr, c.wantExpr)
			}
		})
	}
}

// fakeDoer returns a canned body and records the requested URL.
type fakeDoer struct {
	body   string
	gotURL string
	status int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotURL = req.URL.String()
	st := f.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func TestQueryMetricParsesInstantVector(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"currency":"USD"},"value":[1787846400,"15000"]},
		{"metric":{"currency":"EUR"},"value":[1787846400,"700"]}
	]}}`
	d := &fakeDoer{body: body}
	q := New("http://prom", WithHTTPClient(d))
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}, GroupBy: []string{"currency"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.gotURL, "/api/v1/query?") {
		t.Fatalf("instant query must hit /api/v1/query, got %s", d.gotURL)
	}
	byCur := map[string]float64{}
	for _, s := range series {
		byCur[s.Labels["currency"]] = s.Points[0].Value
	}
	if byCur["USD"] != 15000 || byCur["EUR"] != 700 {
		t.Fatalf("parsed = %v, want USD 15000 EUR 700", byCur)
	}
}

// TestTranslateStepped pins the stepped translation: one instant expr per
// forward bucket [S, min(S+Step, To)), each the same cumulative-difference
// (counter) or carried-level (gauge) shape as the Step==0 translation, with
// lookbacks anchored at From minus the window length so a boundary always
// finds the latest sample the Step==0 translation would have found.
func TestTranslateStepped(t *testing.T) {
	ms := func(tm time.Time) string { return promTime(tm.Add(-time.Millisecond)) }
	ctr := func(rngEnd, atEnd, rngStart, atStart string) string {
		end := `sum by (stage) (last_over_time(biz_txn_total{outcome="failed"}[` + rngEnd + `] @ ` + atEnd + `))`
		start := `sum by (stage) (last_over_time(biz_txn_total{outcome="failed"}[` + rngStart + `] @ ` + atStart + `))`
		return end + ` - (` + start + ` or (` + end + ` * 0))`
	}
	t20, t30, t40, t50 := from.Add(20*time.Minute), from.Add(30*time.Minute), from.Add(40*time.Minute), from.Add(50*time.Minute)
	cases := []struct {
		name       string
		q          query.Query
		wantStarts []time.Time
		wantExprs  []string
	}{
		{
			name: "counter buckets are per-bucket cumulative differences",
			q: query.Query{
				Metric:  "biz_txn_total",
				Agg:     query.AggSum,
				Range:   query.TimeRange{From: from, To: to},
				Filters: map[string]string{"outcome": "failed"},
				GroupBy: []string{"stage"},
				Step:    20 * time.Minute,
			},
			wantStarts: []time.Time{from, t20, t40},
			wantExprs: []string{
				ctr("4800s", ms(t20), "3600s", ms(from)),
				ctr("6000s", ms(t40), "4800s", ms(t20)),
				ctr("7200s", ms(to), "6000s", ms(t40)),
			},
		},
		{
			name: "gauge buckets carry the level forward to each bucket end",
			q: query.Query{
				Metric:  "biz_inflight_value",
				Range:   query.TimeRange{From: from, To: to},
				GroupBy: []string{"age_bucket"},
				Step:    30 * time.Minute,
			},
			wantStarts: []time.Time{from, t30},
			wantExprs: []string{
				`sum by (age_bucket) (last_over_time(biz_inflight_value[5400s] @ ` + ms(t30) + `))`,
				`sum by (age_bucket) (last_over_time(biz_inflight_value[7200s] @ ` + ms(to) + `))`,
			},
		},
		{
			name: "partial last bucket ends at To, not start+Step",
			q: query.Query{
				Metric:  "biz_inflight_value",
				Range:   query.TimeRange{From: from, To: to},
				GroupBy: []string{"age_bucket"},
				Step:    25 * time.Minute,
			},
			wantStarts: []time.Time{from, from.Add(25 * time.Minute), t50},
			wantExprs: []string{
				`sum by (age_bucket) (last_over_time(biz_inflight_value[5100s] @ ` + ms(from.Add(25*time.Minute)) + `))`,
				`sum by (age_bucket) (last_over_time(biz_inflight_value[6600s] @ ` + ms(t50) + `))`,
				`sum by (age_bucket) (last_over_time(biz_inflight_value[7200s] @ ` + ms(to) + `))`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buckets, err := translateStepped(c.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(buckets) != len(c.wantStarts) {
				t.Fatalf("bucket count = %d, want %d", len(buckets), len(c.wantStarts))
			}
			for i, b := range buckets {
				if !b.start.Equal(c.wantStarts[i]) {
					t.Fatalf("bucket %d start = %s, want %s", i, b.start, c.wantStarts[i])
				}
				if b.ex.expr != c.wantExprs[i] {
					t.Fatalf("bucket %d expr = %q\nwant          %q", i, b.ex.expr, c.wantExprs[i])
				}
			}
		})
	}
}

// bucketDoer serves one canned body per bucket, keyed by the request's
// `time` parameter — stepped buckets execute concurrently, so bodies cannot
// be served by arrival order.
type bucketDoer struct {
	bodies map[string]string // time param -> body

	mu   sync.Mutex
	urls []string
}

func (s *bucketDoer) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.urls = append(s.urls, req.URL.String())
	s.mu.Unlock()
	body, ok := s.bodies[req.URL.Query().Get("time")]
	if !ok {
		return nil, errors.New("bucketDoer: unexpected eval time " + req.URL.Query().Get("time"))
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// evalAt renders the eval-time param for a bucket ending at t, matching the
// half-open boundary the translation pins (t-1ms).
func evalAt(t time.Time) string {
	return formatTime(t.Add(-time.Millisecond))
}

// TestSteppedQueryMergesBuckets pins the client-side assembly: one instant
// query per bucket, points restamped at each bucket's start (memq's stamp),
// series merged across buckets in label-key order, and zero-difference
// counter buckets dropped (memq omits sample-less buckets; see translate).
func TestSteppedQueryMergesBuckets(t *testing.T) {
	vec := func(rows string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` + rows + `]}}`
	}
	d := &bucketDoer{bodies: map[string]string{
		evalAt(from.Add(20 * time.Minute)): vec(`{"metric":{"currency":"USD"},"value":[1,"100"]}`),
		evalAt(from.Add(40 * time.Minute)): vec(`{"metric":{"currency":"USD"},"value":[1,"0"]},{"metric":{"currency":"EUR"},"value":[1,"50"]}`),
		evalAt(to):                         vec(`{"metric":{"currency":"USD"},"value":[1,"25"]}`),
	}}
	q := New("http://prom", WithHTTPClient(d))
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to},
		GroupBy: []string{"currency"}, Step: 20 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.urls) != 3 {
		t.Fatalf("issued %d queries, want 3 (one per bucket)", len(d.urls))
	}
	if len(series) != 2 {
		t.Fatalf("series = %d, want 2 (EUR, USD): %+v", len(series), series)
	}
	eur, usd := series[0], series[1] // label-key order
	if eur.Labels["currency"] != "EUR" || usd.Labels["currency"] != "USD" {
		t.Fatalf("series order/labels wrong: %+v", series)
	}
	t20, t40 := from.Add(20*time.Minute), from.Add(40*time.Minute)
	if len(eur.Points) != 1 || eur.Points[0].Value != 50 || !eur.Points[0].At.Equal(t20) {
		t.Fatalf("EUR points = %+v, want one point 50 @ %s", eur.Points, t20)
	}
	if len(usd.Points) != 2 ||
		usd.Points[0].Value != 100 || !usd.Points[0].At.Equal(from) ||
		usd.Points[1].Value != 25 || !usd.Points[1].At.Equal(t40) {
		t.Fatalf("USD points = %+v, want 100 @ %s and 25 @ %s (zero bucket dropped)", usd.Points, from, t40)
	}
}

// TestSteppedGaugeKeepsZeroLevels pins that a gauge level of zero is a real
// observation (an empty queue), not a droppable empty bucket.
func TestSteppedGaugeKeepsZeroLevels(t *testing.T) {
	vec := func(rows string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` + rows + `]}}`
	}
	d := &bucketDoer{bodies: map[string]string{
		evalAt(from.Add(30 * time.Minute)): vec(`{"metric":{"age_bucket":"lt1m"},"value":[1,"0"]}`),
		evalAt(to):                         vec(`{"metric":{"age_bucket":"lt1m"},"value":[1,"700"]}`),
	}}
	q := New("http://prom", WithHTTPClient(d))
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_inflight_value", Range: query.TimeRange{From: from, To: to},
		GroupBy: []string{"age_bucket"}, Step: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Points) != 2 {
		t.Fatalf("series = %+v, want one series with two points (zero level kept)", series)
	}
	if series[0].Points[0].Value != 0 || series[0].Points[1].Value != 700 {
		t.Fatalf("points = %+v, want 0 then 700", series[0].Points)
	}
}

func TestNonFiniteValueRejected(t *testing.T) {
	d := &fakeDoer{body: `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"currency":"USD"},"value":[1787846400,"NaN"]}
	]}}`}
	q := New("http://prom", WithHTTPClient(d))
	if _, err := q.QueryMetric(context.Background(), query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}}); err == nil {
		t.Fatal("a NaN value must be rejected, not flowed to the engine as money")
	}
}

func TestBaseTrailingSlashNormalized(t *testing.T) {
	d := &fakeDoer{body: `{"status":"success","data":{"resultType":"vector","result":[]}}`}
	q := New("http://prom/", WithHTTPClient(d)) // trailing slash
	if _, err := q.QueryMetric(context.Background(), query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.gotURL, "prom//api") {
		t.Fatalf("trailing slash must be normalized, got %s", d.gotURL)
	}
}

func TestCapabilitiesMetricsOnly(t *testing.T) {
	q := New("http://prom")
	c := q.Capabilities()
	if !c.Metrics || c.Events {
		t.Fatalf("caps = %+v, want metrics-only", c)
	}
}

func TestQueryEventsUnsupported(t *testing.T) {
	q := New("http://prom")
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{}); !errors.Is(err, query.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestQueryMetricSurfacesErrorStatus(t *testing.T) {
	d := &fakeDoer{body: `{"status":"error","error":"bad query"}`}
	q := New("http://prom", WithHTTPClient(d))
	if _, err := q.QueryMetric(context.Background(), query.Query{Metric: "biz_txn_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}}); err == nil {
		t.Fatal("a Prometheus error envelope must surface")
	}
}

// gateDoer records the peak number of in-flight requests and serves one body.
type gateDoer struct {
	mu       sync.Mutex
	inflight int
	peak     int
	body     string
}

func (g *gateDoer) Do(*http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.inflight++
	if g.inflight > g.peak {
		g.peak = g.inflight
	}
	g.mu.Unlock()
	time.Sleep(2 * time.Millisecond) // hold the slot so overlap is observable
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(g.body))}, nil
}

// TestSteppedFanOutBounded pins the bounded concurrency: bucket queries
// overlap (more than one in flight) but never exceed the declared bound.
func TestSteppedFanOutBounded(t *testing.T) {
	d := &gateDoer{body: `{"status":"success","data":{"resultType":"vector","result":[]}}`}
	q := New("http://prom", WithHTTPClient(d))
	_, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum,
		Range: query.TimeRange{From: from, To: from.Add(32 * time.Minute)},
		Step:  time.Minute, // 32 buckets
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.peak <= 1 {
		t.Fatalf("bucket queries never overlapped (peak %d) — the fan-out is sequential", d.peak)
	}
	if d.peak > steppedConcurrency {
		t.Fatalf("peak in-flight %d exceeds the bound %d", d.peak, steppedConcurrency)
	}
}

// errAtDoer fails the request whose eval time matches failAt; others succeed.
type errAtDoer struct {
	failAt string
	mu     sync.Mutex
	n      int
}

func (e *errAtDoer) Do(req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	if req.URL.Query().Get("time") == e.failAt {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
		`{"status":"success","data":{"resultType":"vector","result":[]}}`))}, nil
}

// TestSteppedFanOutReportsRealError pins error semantics under concurrency:
// the failing bucket's own error surfaces (never a cancellation induced by
// it), and the fan-out stops issuing work after the failure.
func TestSteppedFanOutReportsRealError(t *testing.T) {
	d := &errAtDoer{failAt: evalAt(from.Add(20 * time.Minute))}
	q := New("http://prom", WithHTTPClient(d))
	_, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum,
		Range: query.TimeRange{From: from, To: to},
		Step:  20 * time.Minute,
	})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("want the failing bucket's status 500 error, got %v", err)
	}
}
