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
	cases := []struct {
		name        string
		q           query.Query
		wantExpr    string
		wantInstant bool
		wantErr     bool
	}{
		{
			name: "counter, whole range, instant increase",
			q: query.Query{Metric: "biz_value_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to},
				Filters: map[string]string{"outcome": "failed", "flow": "invoice.pay"}, GroupBy: []string{"currency"}},
			wantExpr:    `sum by (currency) (increase(biz_value_total{flow="invoice.pay",outcome="failed"}[3600s]))`,
			wantInstant: true,
		},
		{
			name:        "counter, stepped range",
			q:           query.Query{Metric: "biz_txn_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}, Step: time.Minute},
			wantExpr:    `sum (increase(biz_txn_total[60s]))`,
			wantInstant: false,
		},
		{
			name: "gauge reads the level, not increase",
			q: query.Query{Metric: "biz_inflight_value", Range: query.TimeRange{From: from, To: to},
				GroupBy: []string{"age_bucket", "currency"}},
			wantExpr:    `sum by (age_bucket, currency) (biz_inflight_value)`,
			wantInstant: true,
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
			if ex.instant != c.wantInstant {
				t.Fatalf("instant = %v, want %v", ex.instant, c.wantInstant)
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

func TestQueryMetricParsesRangeMatrix(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{},"values":[[1787846400,"100"],[1787846460,"50"]]}
	]}}`
	d := &fakeDoer{body: body}
	q := New("http://prom", WithHTTPClient(d))
	series, err := q.QueryMetric(context.Background(), query.Query{
		Metric: "biz_txn_total", Agg: query.AggSum, Range: query.TimeRange{From: from, To: to}, Step: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.gotURL, "/api/v1/query_range?") {
		t.Fatalf("stepped query must hit /api/v1/query_range, got %s", d.gotURL)
	}
	if len(series) != 1 || len(series[0].Points) != 2 || series[0].Points[0].Value != 100 || series[0].Points[1].Value != 50 {
		t.Fatalf("parsed matrix = %+v", series)
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
