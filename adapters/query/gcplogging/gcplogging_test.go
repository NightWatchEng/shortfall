package gcplogging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/query"
)

var (
	from = time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	to   = time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
)

// TestCapabilitiesAreEventsOnly pins the honest capability declaration:
// Cloud Logging is an event store, and the GCP metric legs come from
// Managed Service for Prometheus through the promql adapter.
func TestCapabilitiesAreEventsOnly(t *testing.T) {
	q, err := New("my-project", "logs_analytics")
	if err != nil {
		t.Fatal(err)
	}
	caps := q.Capabilities()
	if !caps.Events || caps.Metrics {
		t.Fatalf("caps = %+v, want events-only", caps)
	}
	if caps.EventHistoryWeeks != 0 {
		t.Fatalf("EventHistoryWeeks = %d, want 0 (unknown until declared)", caps.EventHistoryWeeks)
	}
	withRetention, err := New("my-project", "logs_analytics", WithEventHistoryWeeks(26))
	if err != nil {
		t.Fatal(err)
	}
	if got := withRetention.Capabilities().EventHistoryWeeks; got != 26 {
		t.Fatalf("declared EventHistoryWeeks = %d, want 26", got)
	}
}

// TestQueryMetricIsUnsupported: the engine must get an honest
// NotAvailable(reason), never a confident zero, for the metric legs.
func TestQueryMetricIsUnsupported(t *testing.T) {
	q, err := New("my-project", "logs_analytics")
	if err != nil {
		t.Fatal(err)
	}
	series, err := q.QueryMetric(context.Background(), query.Query{Metric: "biz_value_total"})
	if !errors.Is(err, query.ErrUnsupported) {
		t.Fatalf("err = %v, want query.ErrUnsupported", err)
	}
	if series != nil {
		t.Fatalf("series = %v, want nil alongside ErrUnsupported", series)
	}
}

// TestGeneratedSQL pins the one reviewed statement: the fully-qualified
// backticked view, the half-open window and every filter value bound as a
// NAMED parameter (never interpolated), the outcome-marker predicate, the
// deterministic order, and the truncation-detecting row cap.
func TestGeneratedSQL(t *testing.T) {
	srv := newFakeBigQuery(t, nil)
	q := srv.querier(t, WithMaxRows(10))
	_, err := q.QueryEvents(context.Background(), query.EventQuery{
		Range:   query.TimeRange{From: from, To: to},
		Filters: map[string]string{"outcome": "failed", "currency": "USD"},
		GroupBy: []string{"customer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sql := srv.lastQuery
	mustContain(t, sql, "FROM `my-project.logs_analytics._AllLogs`")
	mustContain(t, sql, "timestamp >= @window_from AND timestamp < @window_to")
	mustContain(t, sql, `JSON_VALUE(json_payload, '$.event') = @event_marker`)
	mustContain(t, sql, `IFNULL(JSON_VALUE(json_payload, '$."biz.currency"'), '') = @f_currency`)
	mustContain(t, sql, `IFNULL(JSON_VALUE(json_payload, '$."biz.outcome"'), '') = @f_outcome`)
	mustContain(t, sql, "ORDER BY timestamp")
	mustContain(t, sql, "LIMIT 11") // maxRows + 1, so a full page is detectable
	if strings.Contains(sql, "failed") || strings.Contains(sql, "USD") {
		t.Fatalf("filter values were interpolated into the SQL text:\n%s", sql)
	}
	if srv.lastParams["@event_marker"] != "biz.outcome" {
		t.Fatalf("@event_marker = %q", srv.lastParams["@event_marker"])
	}
	if srv.lastParams["@f_outcome"] != "failed" || srv.lastParams["@f_currency"] != "USD" {
		t.Fatalf("filter params = %v", srv.lastParams)
	}
	if got, want := srv.lastParams["@window_from"], "2026-08-25 09:00:00.000000+00:00"; got != want {
		t.Fatalf("@window_from = %q, want %q", got, want)
	}
	if got, want := srv.lastParams["@window_to"], "2026-08-25 10:00:00.000000+00:00"; got != want {
		t.Fatalf("@window_to = %q, want %q", got, want)
	}
	if mode, _ := srv.lastBody["parameterMode"].(string); mode != "NAMED" {
		t.Fatalf("parameterMode = %q, want NAMED", mode)
	}
	if legacy, _ := srv.lastBody["useLegacySql"].(bool); legacy {
		t.Fatal("useLegacySql must be false — the statement is GoogleSQL")
	}
	if loc, _ := srv.lastBody["location"].(string); loc != "us" {
		t.Fatalf("location = %q, want us", loc)
	}
	if srv.lastAuth != "Bearer ya29.test" {
		t.Fatalf("Authorization = %q", srv.lastAuth)
	}
}

// TestWithViewNamesTheView covers the one supported shape knob.
func TestWithViewNamesTheView(t *testing.T) {
	srv := newFakeBigQuery(t, nil)
	q := srv.querier(t, WithView("outcomes_view"))
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{
		Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"currency": "USD"},
	}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, srv.lastQuery, "FROM `my-project.logs_analytics.outcomes_view`")
}

// TestIdentifierValidation: a project, dataset or view name that could
// carry SQL is refused at construction, not quoted-and-hoped.
func TestIdentifierValidation(t *testing.T) {
	cases := []struct {
		name             string
		project, dataset string
		opts             []Option
	}{
		{"backtick in project", "my`project", "d", nil},
		{"space in project", "my project", "d", nil},
		{"empty project", "", "d", nil},
		{"backtick in dataset", "p", "d`x", nil},
		{"dot in dataset", "p", "d.x", nil},
		{"empty dataset", "p", "", nil},
		{"semicolon in view", "p", "d", []Option{WithView("v; DROP TABLE x")}},
		{"backtick in view", "p", "d", []Option{WithView("v`x")}},
		{"empty view", "p", "d", []Option{WithView("")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.project, c.dataset, c.opts...); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
	if _, err := New("my-project", "logs_analytics", WithView("_AllLogs")); err != nil {
		t.Fatalf("legitimate names must be accepted: %v", err)
	}
	if _, err := New("acme.com:legacy-project", "logs_analytics"); err != nil {
		t.Fatalf("domain-scoped project ids must be accepted: %v", err)
	}
}

// TestUnknownLabelIsRefused: an unrecognised filter or group label would
// silently match the empty string and answer the wrong question. Each case
// is a query that is otherwise entirely valid — currency is pinned or
// grouped — so only the unknown label can be what refuses it, and the
// error must name the label rather than being any error at all.
func TestUnknownLabelIsRefused(t *testing.T) {
	srv := newFakeBigQuery(t, nil)
	q := srv.querier(t)
	ctx := context.Background()
	w := query.TimeRange{From: from, To: to}

	_, err := q.QueryEvents(ctx, query.EventQuery{
		Range: w, Filters: map[string]string{"currency": "USD", "provider": "acme"},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown filter label "provider"`) {
		t.Fatalf("err = %v, want an unknown-filter-label refusal naming provider", err)
	}
	_, err = q.QueryEvents(ctx, query.EventQuery{
		Range: w, Filters: map[string]string{"currency": "USD"}, GroupBy: []string{"provider"},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown group label "provider"`) {
		t.Fatalf("err = %v, want an unknown-group-label refusal naming provider", err)
	}
	// Control: the same query with only known labels succeeds, so the two
	// assertions above cannot be passing for an unrelated reason.
	if _, err := q.QueryEvents(ctx, query.EventQuery{
		Range: w, Filters: map[string]string{"currency": "USD"}, GroupBy: []string{"customer"},
	}); err != nil {
		t.Fatalf("control query must succeed: %v", err)
	}
}

// TestCurrencyInvariant: a grouped money read that does not pin or group
// currency is refused, never silently summed across currencies (ADR-0001).
func TestCurrencyInvariant(t *testing.T) {
	events := []biz.Outcome{
		outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(time.Minute)),
		outcome("invoice.pay", "capture", "failed", "inv_2", "h:c2", "smb", "EUR", 5000, from.Add(2*time.Minute)),
	}
	srv := newFakeBigQuery(t, entriesFor(events))
	q := srv.querier(t)
	ctx := context.Background()
	w := query.TimeRange{From: from, To: to}

	for _, qy := range []query.EventQuery{
		{Range: w, GroupBy: []string{"customer"}},
		{Range: w, GroupBy: []string{"customer"}, Agg: query.EventAggMaxPerGroup},
	} {
		if _, err := q.QueryEvents(ctx, qy); err == nil {
			t.Fatalf("cross-currency money read must be refused: %+v", qy)
		}
	}
	// Grouped by currency, or pinned by a filter: allowed, and the two
	// currencies stay separate.
	groups, err := q.QueryEvents(ctx, query.EventQuery{Range: w, GroupBy: []string{"currency"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want one per currency", groups)
	}
	byCur := map[string]int64{}
	for _, g := range groups {
		byCur[g.Key["currency"]] = g.SumMinor
	}
	if byCur["USD"] != 14900 || byCur["EUR"] != 5000 {
		t.Fatalf("per-currency sums = %v", byCur)
	}
	// A distinct count reads no money, so it needs no currency pin.
	if _, err := q.QueryEvents(ctx, query.EventQuery{
		Range: w, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount,
	}); err != nil {
		t.Fatalf("distinct count reads no money and must not need a currency pin: %v", err)
	}
}

// TestPagingAndPollingCollectEveryRow: a stalled job then three pages must
// yield every event, in order — a paging bug silently understates money.
func TestPagingAndPollingCollectEveryRow(t *testing.T) {
	var events []biz.Outcome
	for i := 0; i < 7; i++ {
		events = append(events, outcome("invoice.pay", "capture", "failed",
			fmt.Sprintf("inv_%d", i), "h:c1", "smb", "USD", int64(100*(i+1)), from.Add(time.Duration(i)*time.Minute)))
	}
	srv := newFakeBigQuery(t, entriesFor(events))
	srv.pageSize = 3
	srv.stallOnce = true
	q := srv.querier(t)

	groups, err := q.QueryEvents(context.Background(), query.EventQuery{
		Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"currency": "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want 1", groups)
	}
	// 100+200+...+700
	if groups[0].Count != 7 || groups[0].SumMinor != 2800 {
		t.Fatalf("group = %+v, want count 7 sum 2800", groups[0])
	}
}

// TestTruncationIsLoud: hitting the row cap means the window was cut off
// server-side and any aggregate would understate money — it must be an
// error, not a smaller number.
func TestTruncationIsLoud(t *testing.T) {
	var events []biz.Outcome
	for i := 0; i < 5; i++ {
		events = append(events, outcome("invoice.pay", "capture", "failed",
			fmt.Sprintf("inv_%d", i), "h:c1", "smb", "USD", 100, from.Add(time.Duration(i)*time.Minute)))
	}
	srv := newFakeBigQuery(t, entriesFor(events)) // 5 outcomes + 2 foreign rows
	q := srv.querier(t, WithMaxRows(3))
	_, err := q.QueryEvents(context.Background(), query.EventQuery{
		Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"currency": "USD"},
	})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v, want a loud truncation error", err)
	}
}

// TestFailurePaths pins fail-loud behavior on the wire and on the record:
// an HTTP error, a BigQuery error envelope, a malformed row, and a marked
// outcome record that cannot parse.
func TestFailurePaths(t *testing.T) {
	w := query.TimeRange{From: from, To: to}
	qy := query.EventQuery{Range: w, Filters: map[string]string{"currency": "USD"}}

	t.Run("api error envelope carries the message", func(t *testing.T) {
		srv := newFakeBigQuery(t, nil)
		srv.failStatus = 403
		srv.failBody = errorBody(403, "Access Denied: Table my-project:logs_analytics._AllLogs")
		if _, err := srv.querier(t).QueryEvents(context.Background(), qy); err == nil ||
			!strings.Contains(err.Error(), "Access Denied") {
			t.Fatalf("err = %v, want the API message", err)
		}
	})

	// A non-200 whose body is NOT a Google error envelope — a proxy or load
	// balancer page — is the case only the status check can catch. Without
	// it the decode yields a zero-valued response and the window silently
	// reads as empty, which is the fail-open shape the charter forbids.
	t.Run("non-200 without an error envelope is still an error", func(t *testing.T) {
		srv := newFakeBigQuery(t, nil)
		srv.failStatus = 502
		srv.failBody = "<html><head><title>502 Bad Gateway</title></head></html>"
		_, err := srv.querier(t).QueryEvents(context.Background(), qy)
		if err == nil {
			t.Fatal("a 502 must be an error, never an empty window")
		}
		if !strings.Contains(err.Error(), "502") {
			t.Fatalf("err = %v, want the HTTP status named", err)
		}
	})

	t.Run("marked record that cannot parse fails loudly", func(t *testing.T) {
		srv := newFakeBigQuery(t, []fakeRow{{
			micros:  from.Add(time.Minute).UnixMicro(),
			payload: `{"event":"biz.outcome","biz.flow":"invoice.pay","biz.outcome":"failed","biz.amount_minor":1.5}`,
		}})
		if _, err := srv.querier(t).QueryEvents(context.Background(), qy); err == nil ||
			!strings.Contains(err.Error(), "amount_minor") {
			t.Fatalf("err = %v, want an amount_minor parse failure", err)
		}
	})

	// Both failure envelopes arrive on a 200 too: BigQuery reports
	// job-level failures in `errors` with an OK status, and a gateway can
	// hand back an `error` object with one. Either way the answer is an
	// error, never the empty window a zero-valued decode would look like.
	t.Run("error envelope on a 200 is not an empty window", func(t *testing.T) {
		for _, body := range []string{
			`{"jobComplete":true,"error":{"code":400,"message":"Unrecognized name: json_payload"}}`,
			`{"jobComplete":true,"errors":[{"message":"Query exceeded resource limits"}],"rows":[]}`,
		} {
			_, err := rawQuerier(t, body).QueryEvents(context.Background(), qy)
			if err == nil {
				t.Fatalf("body %s must be an error, not an empty answer", body)
			}
			if !strings.Contains(err.Error(), "Unrecognized name") && !strings.Contains(err.Error(), "resource limits") {
				t.Fatalf("err = %v, want the API message", err)
			}
		}
	})

	t.Run("row with the wrong column count fails loudly", func(t *testing.T) {
		q := rawQuerier(t, `{"jobComplete":true,"rows":[{"f":[{"v":"1787648400000000"}]}]}`)
		if _, err := q.QueryEvents(context.Background(), qy); err == nil ||
			!strings.Contains(err.Error(), "columns") {
			t.Fatalf("err = %v, want a column-count failure", err)
		}
	})

	t.Run("unparsable timestamp fails loudly", func(t *testing.T) {
		q := rawQuerier(t, `{"jobComplete":true,"rows":[{"f":[{"v":"not-a-number"},{"v":"{\"event\":\"biz.outcome\"}"}]}]}`)
		if _, err := q.QueryEvents(context.Background(), qy); err == nil ||
			!strings.Contains(err.Error(), "event_micros") {
			t.Fatalf("err = %v, want an event_micros failure", err)
		}
	})

	t.Run("a job that never completes is an error, not an empty answer", func(t *testing.T) {
		q := rawQuerier(t, `{"jobComplete":false}`)
		if _, err := q.QueryEvents(context.Background(), qy); err == nil ||
			!strings.Contains(err.Error(), "job id") {
			t.Fatalf("err = %v, want a missing-job-id failure", err)
		}
	})

	t.Run("limit without an order is refused", func(t *testing.T) {
		srv := newFakeBigQuery(t, nil)
		_, err := srv.querier(t).QueryEvents(context.Background(), query.EventQuery{
			Range: w, Filters: map[string]string{"currency": "USD"}, Limit: 3,
		})
		if err == nil || !strings.Contains(err.Error(), "OrderBy") {
			t.Fatalf("err = %v, want a Limit-without-OrderBy refusal", err)
		}
	})

	t.Run("token source failure surfaces", func(t *testing.T) {
		srv := newFakeBigQuery(t, nil)
		q, err := New("my-project", "logs_analytics",
			WithEndpoint(srv.srv.URL), WithHTTPClient(srv.srv.Client()),
			WithBearerToken(func() (string, error) { return "", errors.New("metadata server unreachable") }))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.QueryEvents(context.Background(), qy); err == nil ||
			!strings.Contains(err.Error(), "metadata server unreachable") {
			t.Fatalf("err = %v, want the token error", err)
		}
	})
}

// TestForeignEntriesAreSkipped: the _AllLogs view carries every log the
// project writes, so an entry without the outcome marker is not ours and
// must be skipped rather than parsed or counted.
func TestForeignEntriesAreSkipped(t *testing.T) {
	ev := outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(time.Minute))
	srv := newFakeBigQuery(t, []fakeRow{
		{micros: from.Add(time.Minute).UnixMicro(), payload: `{"message":"http 200","latency_ms":12}`},
		{micros: from.Add(time.Minute).UnixMicro(), payload: eventRecord(ev)},
		{micros: from.Add(2 * time.Minute).UnixMicro(), payload: `{"event":"biz.flush","flushed":3}`},
		{micros: from.Add(3 * time.Minute).UnixMicro(), payload: `not json at all`},
	})
	groups, err := srv.querier(t).QueryEvents(context.Background(), query.EventQuery{
		Range: query.TimeRange{From: from, To: to}, Filters: map[string]string{"currency": "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Count != 1 || groups[0].SumMinor != 14900 {
		t.Fatalf("groups = %+v, want exactly the one outcome event", groups)
	}
}
