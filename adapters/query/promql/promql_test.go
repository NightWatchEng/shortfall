package promql

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

var (
	from = time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	to   = time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
)

func TestTranslate(t *testing.T) {
	tT, tF := promTime(to), promTime(from)
	cases := []struct {
		name     string
		q        query.Query
		wantExpr string
		wantErr  bool
	}{
		{
			name: "counter is an exact cumulative difference, not increase()",
			q: query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to},
				Filters: map[string]string{"outcome": "failed", "flow": "invoice.pay"}, GroupBy: []string{"currency"}},
			wantExpr: `sum by (currency) (biz_value_total{flow="invoice.pay",outcome="failed"} @ ` + tT +
				`) - sum by (currency) (biz_value_total{flow="invoice.pay",outcome="failed"} @ ` + tF + `)`,
		},
		{
			name:     "gauge reads the carried-forward level via last_over_time",
			q:        query.Query{Metric: "biz_inflight_value", Range: query.TimeRange{From: from, To: to}, GroupBy: []string{"age_bucket", "currency"}},
			wantExpr: `sum by (age_bucket, currency) (last_over_time(biz_inflight_value[3600s] @ ` + tT + `))`,
		},
		{
			name:    "stepped queries are rejected (bucket alignment)",
			q:       query.Query{Metric: "biz_txn_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}, Step: time.Minute},
			wantErr: true,
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

func TestSteppedQueryRejected(t *testing.T) {
	q := New("http://prom", WithHTTPClient(&fakeDoer{body: `{"status":"success","data":{"resultType":"vector","result":[]}}`}))
	_, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}, Step: time.Minute,
	})
	if err == nil {
		t.Fatal("stepped query must be rejected until parity is proven (workspace-0ka)")
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
