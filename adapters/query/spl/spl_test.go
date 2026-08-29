package spl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func outcome(flow, stage, result, entity, customer, segment, currency string, amount int64, at time.Time) biz.Outcome {
	return biz.Outcome{
		At: at, Stage: stage, Result: biz.Result(result),
		VC: biz.ValueContext{
			Flow: flow, EntityID: entity, CustomerID: customer, Segment: segment, Kind: biz.KindFee,
			Money: biz.Money{Amount: amount, Currency: currency, Exponent: 2},
		},
	}
}

// rawLine renders the HEC event object the way splunkhec ships it (with the
// source_system spelling).
func rawLine(o biz.Outcome) string {
	m := map[string]any{
		"biz.flow": o.VC.Flow, "biz.stage": o.Stage, "biz.outcome": string(o.Result),
		"biz.entity.id": o.VC.EntityID, "biz.customer.id": o.VC.CustomerID,
		"biz.amount_minor": o.VC.Money.Amount, "biz.currency": o.VC.Money.Currency,
		"biz.exponent": 2, "biz.value.kind": "fee", "biz.amount.est": false,
		"biz.segment": o.VC.Segment,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// exportBody renders the export endpoint's NDJSON stream for the events,
// including a preview row the parser must skip.
func exportBody(events []biz.Outcome) string {
	var b strings.Builder
	b.WriteString(`{"preview":true,"result":{"_raw":"ignored","_time":"2026-08-25T09:00:00.000+00:00"}}` + "\n")
	for _, o := range events {
		row := map[string]any{
			"preview": false,
			"result": map[string]string{
				"_raw":  rawLine(o),
				"_time": o.At.Format("2006-01-02T15:04:05.000-07:00"),
			},
		}
		raw, _ := json.Marshal(row)
		b.Write(raw)
		b.WriteString("\n")
	}
	return b.String()
}

// TestQueryEventsMatchesMemq is the reference parity fence over the export
// stream, across the engine's query shapes; it also pins the request shape
// (path, auth, form fields, the one reviewed search string).
func TestQueryEventsMatchesMemq(t *testing.T) {
	events := []biz.Outcome{
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(5*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(9*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_2", "h:c2", "enterprise", "USD", 900000, from.Add(20*time.Minute)),
		outcome("invoice.pay", "settle", "success", "inv_3", "h:c3", "smb", "USD", 5000, from.Add(30*time.Minute)),
	}
	var gotPath, gotAuth, gotSearch, gotEarliest, gotLatest string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.Method + " " + req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		raw, _ := io.ReadAll(req.Body)
		form, _ := parseForm(string(raw))
		gotSearch, gotEarliest, gotLatest = form["search"], form["earliest_time"], form["latest_time"]
		fmt.Fprint(w, exportBody(events))
	}))
	defer srv.Close()

	sq := New(srv.URL, "splunk-token")
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
		got, err := sq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d spl: %v", i, err)
		}
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("query %d parity:\nspl =%+v\nmemq=%+v", i, got, want)
		}
	}
	if gotPath != "POST /services/search/jobs/export" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer splunk-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if want := `search index="main" sourcetype="shortfall:outcome" | fields _time _raw`; gotSearch != want {
		t.Fatalf("search = %q, want %q", gotSearch, want)
	}
	// Bounds widen a second each side (Splunk latest is exclusive).
	if gotEarliest != "1787648399" || gotLatest != "1787652001" {
		t.Fatalf("window = %s..%s", gotEarliest, gotLatest)
	}
}

// parseForm decodes an application/x-www-form-urlencoded body.
func parseForm(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, "&") {
		k, v, _ := strings.Cut(kv, "=")
		dv, err := url.QueryUnescape(v)
		if err != nil {
			return nil, err
		}
		out[k] = dv
	}
	return out, nil
}

// TestUnsupportedMetricsAndErrors pins capability honesty and fail-loud
// paths (HTTP status, foreign raw line, unparsable _time).
func TestUnsupportedMetricsAndErrors(t *testing.T) {
	sq := New("http://splunk", "t")
	if !sq.Capabilities().Events || sq.Capabilities().Metrics {
		t.Fatal("caps must be events-only")
	}
	if _, err := sq.QueryMetric(context.Background(), query.Query{}); err != query.ErrUnsupported {
		t.Fatalf("QueryMetric err = %v, want ErrUnsupported", err)
	}

	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name:    "non-200 status",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) },
			wantErr: "503",
		},
		{
			name: "foreign raw line fails loudly",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"preview":false,"result":{"_raw":"{\"level\":\"info\"}","_time":"2026-08-25T09:01:00.000+00:00"}}`+"\n")
			},
			wantErr: "not a biz outcome",
		},
		{
			name: "unparsable time fails loudly",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"preview":false,"result":{"_raw":"{}","_time":"yesterday"}}`+"\n")
			},
			wantErr: "_time",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			bad := New(srv.URL, "t")
			_, err := bad.QueryEvents(context.Background(), query.EventQuery{Range: query.TimeRange{From: from, To: to}})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}
