package logql

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
)

var (
	from = time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	to   = time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
)

// lokiLine renders an outcome the way the loki exporter does.
func lokiLine(flow, stage, outcome, entity, customer, segment, currency string, amount int64) string {
	m := map[string]any{
		"biz.flow": flow, "biz.stage": stage, "biz.outcome": outcome,
		"biz.entity.id": entity, "biz.customer.id": customer,
		"biz.amount_minor": amount, "biz.currency": currency, "biz.exponent": 2,
		"biz.value.kind": "fee", "biz.amount.est": false, "biz.segment": segment,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func outcome(flow, stage, result, entity, customer, segment, currency string, amount int64, at time.Time) biz.Outcome {
	return biz.Outcome{
		At: at, Stage: stage, Result: biz.Result(result),
		VC: biz.ValueContext{
			Flow: flow, EntityID: entity, CustomerID: customer, Segment: segment, Kind: biz.KindFee,
			Money: biz.Money{Amount: amount, Currency: currency, Exponent: 2},
		},
	}
}

// streamsBody renders a query_range response holding the given entries.
func streamsBody(entries [][2]string) string {
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "streams",
			"result":     []map[string]any{{"stream": map[string]string{}, "values": entries}},
		},
	})
	return string(b)
}

// TestSelectorPushdown pins the stream-label pushdown: flow/stage/outcome
// filters become selector equality; anything else stays client-side; no
// filters selects every shortfall stream.
func TestSelectorPushdown(t *testing.T) {
	cases := []struct {
		name    string
		filters map[string]string
		want    string
	}{
		{"no filters", nil, `{outcome=~".+"}`},
		{"labels push down", map[string]string{"flow": "invoice.pay", "outcome": "failed"}, `{flow="invoice.pay",outcome="failed"}`},
		{"line fields stay client-side", map[string]string{"currency": "USD", "customer": "h:c1"}, `{outcome=~".+"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := selector(c.filters); got != c.want {
				t.Fatalf("selector = %q, want %q", got, c.want)
			}
		})
	}
}

// TestQueryEventsMatchesMemq is the reference parity fence: the same
// outcomes served as Loki lines must aggregate identically to memq fed the
// outcomes directly, across the engine's query shapes.
func TestQueryEventsMatchesMemq(t *testing.T) {
	events := []biz.Outcome{
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(5*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(9*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_2", "h:c2", "enterprise", "USD", 900000, from.Add(20*time.Minute)),
		outcome("invoice.pay", "settle", "success", "inv_3", "h:c3", "smb", "USD", 5000, from.Add(30*time.Minute)),
	}
	var entries [][2]string
	for _, o := range events {
		entries = append(entries, [2]string{
			strconv.FormatInt(o.At.UnixNano(), 10),
			lokiLine(o.VC.Flow, o.Stage, string(o.Result), o.VC.EntityID, o.VC.CustomerID, o.VC.Segment, o.VC.Money.Currency, o.VC.Money.Amount),
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, streamsBody(entries))
	}))
	defer srv.Close()

	lq := New(srv.URL)
	mq := memq.New(memq.WithEvents(events))
	ctx := context.Background()
	queries := []query.EventQuery{
		{Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"currency", "entity"}, Agg: query.EventAggMaxPerGroup},
		{Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount},
		{Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer", "segment"}},
	}
	for i, qy := range queries {
		want, err := mq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d memq: %v", i, err)
		}
		got, err := lq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d logql: %v", i, err)
		}
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("query %d parity:\nlogql=%+v\nmemq =%+v", i, got, want)
		}
	}
}

// TestPagination pins forward paging: a full first page advances start past
// the last timestamp; the second page's remainder is appended.
func TestPagination(t *testing.T) {
	tsA := from.Add(1 * time.Minute)
	tsB := from.Add(2 * time.Minute)
	lineFor := func(entity string, at time.Time) [2]string {
		return [2]string{
			strconv.FormatInt(at.UnixNano(), 10),
			lokiLine("invoice.pay", "capture", "failed", entity, "h:c1", "smb", "USD", 100),
		}
	}
	// The fake respects start the way Loki does: entries strictly before the
	// requested start are not re-served, so the third page is empty and the
	// pager terminates.
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		starts = append(starts, req.URL.Query().Get("start"))
		start, _ := strconv.ParseInt(req.URL.Query().Get("start"), 10, 64)
		var page [][2]string
		for _, e := range [][2]any{{tsA, "inv_1"}, {tsB, "inv_2"}} {
			at := e[0].(time.Time)
			if at.UnixNano() >= start {
				page = append(page, lineFor(e[1].(string), at))
				break // limit 1 per page
			}
		}
		fmt.Fprint(w, streamsBody(page))
	}))
	defer srv.Close()

	lq := New(srv.URL, WithPageLimit(1))
	groups, err := lq.QueryEvents(context.Background(), query.EventQuery{
		Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"currency": "USD"},
		GroupBy: []string{"entity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want the two paged entities", groups)
	}
	if want := strconv.FormatInt(tsA.UnixNano()+1, 10); len(starts) < 2 || starts[1] != want {
		t.Fatalf("second page start = %v, want %s", starts, want)
	}
}

// TestForeignLineFailsLoudly pins the no-silent-skip contract: a non-outcome
// line in the selected streams is a misconfiguration, not data to drop.
func TestForeignLineFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, streamsBody([][2]string{{strconv.FormatInt(from.UnixNano(), 10), `{"level":"info"}`}}))
	}))
	defer srv.Close()
	lq := New(srv.URL)
	_, err := lq.QueryEvents(context.Background(), query.EventQuery{Range: query.TimeRange{From: from, To: to}})
	if err == nil || !strings.Contains(err.Error(), "not a biz outcome") {
		t.Fatalf("want loud parse failure, got %v", err)
	}
}

// TestUnsupportedMetricsAndErrors pins the capability honesty and fail-loud
// HTTP error path.
func TestUnsupportedMetricsAndErrors(t *testing.T) {
	lq := New("http://loki")
	if !lq.Capabilities().Events || lq.Capabilities().Metrics {
		t.Fatal("caps must be events-only")
	}
	if _, err := lq.QueryMetric(context.Background(), query.Query{}); err != query.ErrUnsupported {
		t.Fatalf("QueryMetric err = %v, want ErrUnsupported", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	bad := New(srv.URL)
	if _, err := bad.QueryEvents(context.Background(), query.EventQuery{Range: query.TimeRange{From: from, To: to}}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status error, got %v", err)
	}
}
