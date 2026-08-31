// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package cwinsights

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// emfEventMessage renders an outcome the way the cloudwatch exporter's
// event records look (with the biz.outcome marker and _aws envelope).
func emfEventMessage(o biz.Outcome) string {
	m := map[string]any{
		"_aws": map[string]any{"Timestamp": o.At.UnixMilli()}, "event": "biz.outcome",
		"biz.flow": o.VC.Flow, "biz.stage": o.Stage, "biz.outcome": string(o.Result),
		"biz.entity.id": o.VC.EntityID, "biz.customer.id": o.VC.CustomerID,
		"biz.amount.minor": o.VC.Money.Amount, "biz.amount.currency": o.VC.Money.Currency,
		"biz.amount.exponent": 2, "biz.value.kind": "fee", "biz.amount.estimated": false,
		"biz.segment": o.VC.Segment,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// resultsBody renders a Complete GetQueryResults response: the outcome rows
// plus one EMF metric record the parser must skip by its missing marker.
func resultsBody(events []biz.Outcome) string {
	rows := [][]map[string]any{}
	metricRecord := `{"_aws":{"Timestamp":1787648400000,"CloudWatchMetrics":[]},"biz_txn_total":1}`
	rows = append(rows, []map[string]any{
		{"field": "@message", "value": metricRecord},
		{"field": "@timestamp", "value": 1787648400000},
	})
	for _, o := range events {
		rows = append(rows, []map[string]any{
			{"field": "@message", "value": emfEventMessage(o)},
			{"field": "@timestamp", "value": o.At.UnixMilli()},
			{"field": "@ptr", "value": 0},
		})
	}
	b, _ := json.Marshal(map[string]any{"status": "Complete", "results": rows})
	return string(b)
}

// TestQueryEventsMatchesMemq is the reference parity fence over the
// Insights round trip (StartQuery -> poll -> Complete), across the engine's
// query shapes; it also pins the JSON-RPC request shape and the SigV4
// header presence.
func TestQueryEventsMatchesMemq(t *testing.T) {
	events := []biz.Outcome{
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(5*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(9*time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_2", "h:c2", "enterprise", "USD", 900000, from.Add(20*time.Minute)),
		outcome("invoice.pay", "settle", "success", "inv_3", "h:c3", "smb", "USD", 5000, from.Add(30*time.Minute)),
	}
	var startBody map[string]any
	var gotTargets []string
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		target := req.Header.Get("X-Amz-Target")
		gotTargets = append(gotTargets, target)
		if auth := req.Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=test/") {
			t.Errorf("missing sigv4 auth, got %q", auth)
		}
		raw, _ := io.ReadAll(req.Body)
		switch target {
		case "Logs_20140328.StartQuery":
			_ = json.Unmarshal(raw, &startBody)
			_, _ = fmt.Fprint(w, `{"queryId":"q-1"}`)
		case "Logs_20140328.GetQueryResults":
			polls++
			if polls == 1 {
				_, _ = fmt.Fprint(w, `{"status":"Running","results":[]}`)
				return
			}
			_, _ = fmt.Fprint(w, resultsBody(events))
		default:
			t.Errorf("unexpected target %q", target)
		}
	}))
	defer srv.Close()

	cq := New("us-east-1", "/shortfall/prod", "test", "secret",
		WithEndpoint(srv.URL), WithPollInterval(time.Millisecond))
	mq := memq.New(memq.WithEvents(events))
	ctx := context.Background()
	queries := []query.EventQuery{
		{Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"currency", "entity"}, Agg: query.EventAggMaxPerGroup},
		{Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount},
		{Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer", "segment"}},
	}
	for i, qy := range queries {
		polls = 0
		want, err := mq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d memq: %v", i, err)
		}
		got, err := cq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d cwinsights: %v", i, err)
		}
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("query %d parity:\ncw  =%+v\nmemq=%+v", i, got, want)
		}
	}
	if startBody["logGroupName"] != "/shortfall/prod" {
		t.Fatalf("logGroupName = %v", startBody["logGroupName"])
	}
	if qs, _ := startBody["queryString"].(string); !strings.Contains(qs, `filter event = "biz.outcome"`) {
		t.Fatalf("queryString = %q", qs)
	}
}

// TestSigV4KnownVector pins the signer against AWS's published get-vanilla
// test vector (access key AKIDEXAMPLE, service "service", 2015-08-30).
func TestSigV4KnownVector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	req.Host = "example.amazonaws.com"
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	signV4svc(req, nil, credentials{
		AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}, "us-east-1", "service", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization =\n%s\nwant\n%s", got, want)
	}
}

// TestFailurePaths pins fail-loud behavior: a Failed query status, a marked
// outcome row that cannot parse, and capability honesty.
func TestFailurePaths(t *testing.T) {
	cq := New("us-east-1", "/g", "k", "s")
	if !cq.Capabilities().Events || cq.Capabilities().Metrics {
		t.Fatal("caps must be events-only")
	}
	if _, err := cq.QueryMetric(context.Background(), query.Query{}); err != query.ErrUnsupported {
		t.Fatalf("QueryMetric err = %v, want ErrUnsupported", err)
	}

	cases := []struct {
		name    string
		results string
		wantErr string
	}{
		{
			name:    "failed status",
			results: `{"status":"Failed","results":[]}`,
			wantErr: `status "Failed"`,
		},
		{
			name: "marked outcome row that cannot parse fails loudly",
			results: `{"status":"Complete","results":[[` +
				`{"field":"@message","value":"{\"event\":\"biz.outcome\",\"biz.flow\":\"f\",\"biz.outcome\":\"failed\",\"biz.amount.minor\":1.5}"},` +
				`{"field":"@timestamp","value":1787648400000}]]}`,
			wantErr: "amount_minor",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("X-Amz-Target") == "Logs_20140328.StartQuery" {
					_, _ = fmt.Fprint(w, `{"queryId":"q-1"}`)
					return
				}
				_, _ = fmt.Fprint(w, c.results)
			}))
			defer srv.Close()
			bad := New("us-east-1", "/g", "k", "s", WithEndpoint(srv.URL), WithPollInterval(time.Millisecond))
			_, err := bad.QueryEvents(context.Background(), query.EventQuery{Range: query.TimeRange{From: from, To: to}})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}
