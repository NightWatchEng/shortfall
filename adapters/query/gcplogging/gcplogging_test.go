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

// window is the standard half-open read window for the unit tests.
func window() query.TimeRange { return query.TimeRange{From: from, To: to} }

// TestCapabilities pins the honest capability declaration: Cloud Logging is
// an event store, and the GCP metric legs come from Managed Service for
// Prometheus through the promql adapter.
func TestCapabilities(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want query.Caps
	}{
		{
			name: "default: events only, retention unknown",
			want: query.Caps{Metrics: false, Events: true, EventHistoryWeeks: 0},
		},
		{
			name: "retention declared by the operator",
			opts: []Option{WithEventHistoryWeeks(26)},
			want: query.Caps{Metrics: false, Events: true, EventHistoryWeeks: 26},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := New("my-project", "logs_analytics", c.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := q.Capabilities(); got != c.want {
				t.Fatalf("caps = %+v, want %+v", got, c.want)
			}
		})
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
		Range:   window(),
		Filters: map[string]string{"outcome": "failed", "currency": "USD"},
		GroupBy: []string{"customer"},
	})
	if err != nil {
		t.Fatal(err)
	}

	fragments := []struct {
		name string
		want string
	}{
		{"fully-qualified backticked view", "FROM `my-project.logs_analytics._AllLogs`"},
		{"half-open window on the entry timestamp", "timestamp >= @window_from AND timestamp < @window_to"},
		{"outcome marker predicate", `JSON_VALUE(json_payload, '$.event') = @event_marker`},
		{"currency filter, IFNULL-guarded and bound", `IFNULL(JSON_VALUE(json_payload, '$."biz.currency"'), '') = @f_currency`},
		{"outcome filter, IFNULL-guarded and bound", `IFNULL(JSON_VALUE(json_payload, '$."biz.outcome"'), '') = @f_outcome`},
		{"deterministic order", "ORDER BY timestamp"},
		{"row cap one above maxRows so a full page is detectable", "LIMIT 11"},
	}
	for _, f := range fragments {
		t.Run(f.name, func(t *testing.T) { mustContain(t, srv.lastQuery, f.want) })
	}

	params := []struct {
		name  string
		param string
		want  string
	}{
		{"marker bound, not inlined", "@event_marker", "biz.outcome"},
		{"outcome value bound", "@f_outcome", "failed"},
		{"currency value bound", "@f_currency", "USD"},
		{"window start as a BigQuery TIMESTAMP literal", "@window_from", "2026-08-25 09:00:00.000000+00:00"},
		{"window end as a BigQuery TIMESTAMP literal", "@window_to", "2026-08-25 10:00:00.000000+00:00"},
	}
	for _, p := range params {
		t.Run(p.name, func(t *testing.T) {
			if got := srv.lastParams[p.param]; got != p.want {
				t.Fatalf("%s = %q, want %q", p.param, got, p.want)
			}
		})
	}

	t.Run("no filter value reaches the SQL text", func(t *testing.T) {
		if strings.Contains(srv.lastQuery, "failed") || strings.Contains(srv.lastQuery, "USD") {
			t.Fatalf("filter values were interpolated into the SQL text:\n%s", srv.lastQuery)
		}
	})
	t.Run("request envelope", func(t *testing.T) {
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
	})
}

// TestWindowBoundsStayASuperset pins the direction each bound rounds. The
// pushdown is only safe because memq re-applies the exact half-open
// [From, To) afterwards, and that argument holds only while the fetch is a
// superset: BigQuery TIMESTAMP is microsecond-resolution, so From must
// round DOWN and To must round UP. A To that truncated downward would drop
// an entry at exactly the truncated microsecond that the reference admits,
// and no client-side re-filter can recover a row that was never fetched.
func TestWindowBoundsStayASuperset(t *testing.T) {
	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		from, to       time.Time
		wantFrom, want string
	}{
		{
			name:     "microsecond-aligned bounds are unchanged",
			from:     base,
			to:       base.Add(time.Hour),
			wantFrom: "2026-08-25 09:00:00.000000+00:00",
			want:     "2026-08-25 10:00:00.000000+00:00",
		},
		{
			name:     "sub-microsecond start rounds down, widening",
			from:     base.Add(500 * time.Nanosecond),
			to:       base.Add(time.Hour),
			wantFrom: "2026-08-25 09:00:00.000000+00:00",
			want:     "2026-08-25 10:00:00.000000+00:00",
		},
		{
			name:     "sub-microsecond end rounds up, widening",
			from:     base,
			to:       base.Add(time.Hour).Add(500 * time.Nanosecond),
			wantFrom: "2026-08-25 09:00:00.000000+00:00",
			want:     "2026-08-25 10:00:00.000001+00:00",
		},
		{
			name:     "end one nanosecond past a microsecond still rounds up",
			from:     base,
			to:       base.Add(time.Hour).Add(time.Nanosecond),
			wantFrom: "2026-08-25 09:00:00.000000+00:00",
			want:     "2026-08-25 10:00:00.000001+00:00",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newFakeBigQuery(t, nil)
			q := srv.querier(t)
			if _, err := q.QueryEvents(context.Background(), query.EventQuery{
				Range: query.TimeRange{From: c.from, To: c.to}, Filters: map[string]string{"currency": "USD"},
			}); err != nil {
				t.Fatal(err)
			}
			if got := srv.lastParams["@window_from"]; got != c.wantFrom {
				t.Fatalf("@window_from = %q, want %q", got, c.wantFrom)
			}
			if got := srv.lastParams["@window_to"]; got != c.want {
				t.Fatalf("@window_to = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSubMicrosecondWindowKeepsTheBoundaryEvent is the behavioural half of
// the bound-rounding contract: an entry stored at exactly the microsecond
// the window end truncates to is inside memq's [From, To), so the adapter
// must return it too.
func TestSubMicrosecondWindowKeepsTheBoundaryEvent(t *testing.T) {
	edge := from.Add(time.Minute)          // microsecond-aligned
	end := edge.Add(500 * time.Nanosecond) // To truncates down to edge
	ev := outcome("invoice.pay", "capture", "failed", "inv_edge", "h:c1", "smb", "USD", 14900, edge)
	srv := newFakeBigQuery(t, entriesFor([]biz.Outcome{ev}))
	groups, err := srv.querier(t).QueryEvents(context.Background(), query.EventQuery{
		Range: query.TimeRange{From: from, To: end}, Filters: map[string]string{"currency": "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Count != 1 || groups[0].SumMinor != 14900 {
		t.Fatalf("groups = %+v, want the boundary event (the reference admits it: %v < %v)",
			groups, edge, end)
	}
}

// TestServerWaitFitsInsideTheClientDeadline pins the ordering of the two
// deadlines. If the server-side wait matched the default client timeout,
// the jobComplete:false answer would arrive no earlier than the client
// gives up, making the polling loop unreachable with the adapter's own
// default doer.
func TestServerWaitFitsInsideTheClientDeadline(t *testing.T) {
	if serverWait >= defaultHTTPTimeout {
		t.Fatalf("serverWait %v must be below the client deadline %v", serverWait, defaultHTTPTimeout)
	}
	if margin := defaultHTTPTimeout - serverWait; margin < 10*time.Second {
		t.Fatalf("margin %v is too tight: connect, TLS and body read all share the client deadline", margin)
	}
	srv := newFakeBigQuery(t, nil)
	if _, err := srv.querier(t).QueryEvents(context.Background(), query.EventQuery{
		Range: window(), Filters: map[string]string{"currency": "USD"},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := srv.lastBody["timeoutMs"].(float64); int64(got) != serverWait.Milliseconds() {
		t.Fatalf("request timeoutMs = %v, want serverWait %v", got, serverWait.Milliseconds())
	}
}

// TestWithViewNamesTheView covers the one supported shape knob.
func TestWithViewNamesTheView(t *testing.T) {
	srv := newFakeBigQuery(t, nil)
	q := srv.querier(t, WithView("outcomes_view"))
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{
		Range: window(), Filters: map[string]string{"currency": "USD"},
	}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, srv.lastQuery, "FROM `my-project.logs_analytics.outcomes_view`")
}

// TestIdentifierValidation: a project, dataset or view name that could
// carry SQL is refused at construction, not quoted-and-hoped. The rejection
// rows assert WHICH identifier was refused (ADR-0007) — an accepted view
// name reported as an invalid dataset would be a silently wrong diagnosis.
func TestIdentifierValidation(t *testing.T) {
	cases := []struct {
		name             string
		project, dataset string
		opts             []Option
		wantErr          string
	}{
		{name: "backtick in project", project: "my`project", dataset: "d", wantErr: `invalid project id "my`},
		{name: "space in project", project: "my project", dataset: "d", wantErr: `invalid project id "my project"`},
		{name: "quote in project", project: `my'project`, dataset: "d", wantErr: "invalid project id"},
		{name: "empty project", project: "", dataset: "d", wantErr: `invalid project id ""`},
		{name: "backtick in dataset", project: "p", dataset: "d`x", wantErr: "invalid dataset id"},
		{name: "dot in dataset", project: "p", dataset: "d.x", wantErr: `invalid dataset id "d.x"`},
		{name: "empty dataset", project: "p", dataset: "", wantErr: `invalid dataset id ""`},
		{name: "semicolon in view", project: "p", dataset: "d", opts: []Option{WithView("v; DROP TABLE x")}, wantErr: "invalid view id"},
		{name: "backtick in view", project: "p", dataset: "d", opts: []Option{WithView("v`x")}, wantErr: "invalid view id"},
		{name: "empty view", project: "p", dataset: "d", opts: []Option{WithView("")}, wantErr: `invalid view id ""`},
		{name: "non-positive max rows", project: "p", dataset: "d", opts: []Option{WithMaxRows(0)}, wantErr: "max rows must be positive"},
		{name: "zero poll interval", project: "p", dataset: "d", opts: []Option{WithPollInterval(0)}, wantErr: "poll interval must be positive"},
		{name: "negative poll interval", project: "p", dataset: "d", opts: []Option{WithPollInterval(-time.Second)}, wantErr: "poll interval must be positive"},
		{name: "legitimate names accepted", project: "my-project", dataset: "logs_analytics", opts: []Option{WithView("_AllLogs")}},
		{name: "domain-scoped project id accepted", project: "acme.com:legacy-project", dataset: "logs_analytics"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.project, c.dataset, c.opts...)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

// TestUnknownLabelIsRefused: an unrecognised filter or group label would
// silently match the empty string and answer the wrong question. Each
// rejection case is a query that is otherwise entirely valid — currency is
// pinned — so only the unknown label can be what refuses it, and the error
// must name the label rather than being any error at all.
func TestUnknownLabelIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		q       query.EventQuery
		wantErr string
	}{
		{
			name:    "unknown filter label",
			q:       query.EventQuery{Range: window(), Filters: map[string]string{"currency": "USD", "provider": "acme"}},
			wantErr: `unknown filter label "provider"`,
		},
		{
			name:    "unknown group label",
			q:       query.EventQuery{Range: window(), Filters: map[string]string{"currency": "USD"}, GroupBy: []string{"provider"}},
			wantErr: `unknown group label "provider"`,
		},
		{
			// Control: without it the two rows above could be passing for
			// an unrelated reason.
			name: "known labels only",
			q:    query.EventQuery{Range: window(), Filters: map[string]string{"currency": "USD"}, GroupBy: []string{"customer"}},
		},
	}
	srv := newFakeBigQuery(t, nil)
	q := srv.querier(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := q.QueryEvents(context.Background(), c.q)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
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

	refused := []struct {
		name string
		q    query.EventQuery
	}{
		{"grouped sum without currency", query.EventQuery{Range: window(), GroupBy: []string{"customer"}}},
		{"max-per-group without currency", query.EventQuery{Range: window(), GroupBy: []string{"customer"}, Agg: query.EventAggMaxPerGroup}},
		{"ungrouped sum without currency", query.EventQuery{Range: window()}},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			if _, err := q.QueryEvents(ctx, c.q); err == nil {
				t.Fatal("a cross-currency money read must be refused")
			}
		})
	}

	t.Run("grouping by currency keeps the two currencies apart", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{Range: window(), GroupBy: []string{"currency"}})
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
			t.Fatalf("per-currency sums = %v, want USD 14900 and EUR 5000", byCur)
		}
	})

	t.Run("distinct count reads no money and needs no currency pin", func(t *testing.T) {
		groups, err := q.QueryEvents(ctx, query.EventQuery{
			Range: window(), GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Count != 2 {
			t.Fatalf("groups = %+v, want one group counting 2 distinct customers", groups)
		}
	})
}

// TestPagingAndPollingCollectEveryRow: a stalled job then three pages must
// yield every event — a paging bug silently understates money.
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
		Range: window(), Filters: map[string]string{"currency": "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want 1", groups)
	}
	if groups[0].Count != 7 || groups[0].SumMinor != 2800 { // 100+200+...+700
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
	srv := newFakeBigQuery(t, entriesFor(events))
	cases := []struct {
		name    string
		maxRows int
		wantErr string
	}{
		{name: "cap below the window's row count", maxRows: 3, wantErr: "truncated"},
		{name: "cap exactly at the row count still fits", maxRows: 5},
		{name: "cap above the row count fits", maxRows: 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := srv.querier(t, WithMaxRows(c.maxRows)).QueryEvents(context.Background(), query.EventQuery{
				Range: window(), Filters: map[string]string{"currency": "USD"},
			})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want a loud truncation error", err)
			}
		})
	}
}

// TestFailurePaths pins fail-loud behavior on the wire and on the record.
// Every case reduces to one shape — build a querier over a backend that
// misbehaves in one specific way, run one query, and require an error
// naming the cause — so it is a table with one assertion body (ADR-0007).
// Nothing here may answer "no rows": an unreadable backend that looked like
// an empty window would be reported as a measured zero.
func TestFailurePaths(t *testing.T) {
	usd := query.EventQuery{Range: window(), Filters: map[string]string{"currency": "USD"}}

	fakeWith := func(configure func(*fakeBigQuery)) func(*testing.T) *Querier {
		return func(t *testing.T) *Querier {
			srv := newFakeBigQuery(t, nil)
			configure(srv)
			return srv.querier(t)
		}
	}
	raw := func(body string) func(*testing.T) *Querier {
		return func(t *testing.T) *Querier { return rawQuerier(t, body) }
	}

	cases := []struct {
		name    string
		build   func(*testing.T) *Querier
		q       query.EventQuery
		wantErr string
	}{
		{
			name: "api error envelope on a non-200 carries the message",
			build: fakeWith(func(f *fakeBigQuery) {
				f.failStatus = 403
				f.failBody = errorBody(403, "Access Denied: Table my-project:logs_analytics._AllLogs")
			}),
			q:       usd,
			wantErr: "Access Denied",
		},
		{
			// Only the status check catches a non-200 whose body is not a
			// Google error envelope — a proxy or load-balancer page. Without
			// it the decode yields a zero-valued response that reads as an
			// empty window.
			name: "non-200 without an error envelope names the status",
			build: fakeWith(func(f *fakeBigQuery) {
				f.failStatus = 502
				f.failBody = "<html><head><title>502 Bad Gateway</title></head></html>"
			}),
			q:       usd,
			wantErr: "502",
		},
		{
			// BigQuery reports job-level failures in `errors` with an OK
			// status, so a 200 is not evidence of an answer.
			name:    "job errors on a 200 are not an empty window",
			build:   raw(`{"jobComplete":true,"errors":[{"message":"Query exceeded resource limits"}],"rows":[]}`),
			q:       usd,
			wantErr: "resource limits",
		},
		{
			name:    "error object on a 200 is not an empty window",
			build:   raw(`{"jobComplete":true,"error":{"code":400,"message":"Unrecognized name: json_payload"}}`),
			q:       usd,
			wantErr: "Unrecognized name",
		},
		{
			name:    "row with the wrong column count",
			build:   raw(`{"jobComplete":true,"rows":[{"f":[{"v":"1787648400000000"}]}]}`),
			q:       usd,
			wantErr: "columns",
		},
		{
			name:    "unparsable event_micros",
			build:   raw(`{"jobComplete":true,"rows":[{"f":[{"v":"not-a-number"},{"v":"{\"event\":\"biz.outcome\"}"}]}]}`),
			q:       usd,
			wantErr: "event_micros",
		},
		{
			name:    "job that never completes and names no job id",
			build:   raw(`{"jobComplete":false}`),
			q:       usd,
			wantErr: "job id",
		},
		{
			// A marked record that cannot be decoded is a truncated or
			// corrupted outcome; counting it as nothing would understate
			// money, so it is loud rather than skipped.
			name: "marked record that cannot parse",
			build: fakeWith(func(f *fakeBigQuery) {
				f.rows = []fakeRow{{
					micros: from.Add(time.Minute).UnixMicro(),
					payload: `{"event":"biz.outcome","biz.flow":"invoice.pay","biz.outcome":"failed",` +
						`"biz.currency":"USD","biz.amount_minor":1.5}`,
				}}
			}),
			q:       usd,
			wantErr: "amount_minor",
		},
		{
			name:    "limit without an order",
			build:   fakeWith(func(*fakeBigQuery) {}),
			q:       query.EventQuery{Range: window(), Filters: map[string]string{"currency": "USD"}, Limit: 3},
			wantErr: "OrderBy",
		},
		{
			name: "token source failure",
			build: func(t *testing.T) *Querier {
				srv := newFakeBigQuery(t, nil)
				q, err := New("my-project", "logs_analytics",
					WithEndpoint(srv.srv.URL), WithHTTPClient(srv.srv.Client()),
					WithBearerToken(func() (string, error) { return "", errors.New("metadata server unreachable") }))
				if err != nil {
					t.Fatal(err)
				}
				return q
			},
			q:       usd,
			wantErr: "metadata server unreachable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			groups, err := c.build(t).QueryEvents(context.Background(), c.q)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
			if groups != nil {
				t.Fatalf("groups = %+v, want nil alongside the error", groups)
			}
		})
	}
}

// TestForeignEntriesAreSkipped: the log bucket's view carries every log the
// project writes. The SQL filters them out server-side, but a backend that
// ignores the pushdown (evaluate=false here) hands them straight to the
// decoder, which must skip anything without the outcome marker rather than
// failing the read or counting it.
func TestForeignEntriesAreSkipped(t *testing.T) {
	ev := outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(time.Minute))
	srv := newFakeBigQuery(t, []fakeRow{
		{micros: from.Add(time.Minute).UnixMicro(), payload: `{"message":"http 200","latency_ms":12}`},
		{micros: from.Add(time.Minute).UnixMicro(), payload: eventRecord(ev)},
		{micros: from.Add(2 * time.Minute).UnixMicro(), payload: `{"event":"biz.flush","flushed":3}`},
		{micros: from.Add(3 * time.Minute).UnixMicro(), payload: `not json at all`},
	})
	srv.evaluate = false // the backend hands back everything it holds
	groups, err := srv.querier(t).QueryEvents(context.Background(), query.EventQuery{
		Range: window(), Filters: map[string]string{"currency": "USD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Count != 1 || groups[0].SumMinor != 14900 {
		t.Fatalf("groups = %+v, want exactly the one outcome event", groups)
	}
}

// TestWrongPayloadColumnIsLoud pins the fail-open shape the marker
// discrimination could otherwise hide. Skipping an entry that is not a JSON
// object is right for a plain text log line, but a result set in which NOT
// ONE row is an object is not a quiet window — it is the wrong column.
// TO_JSON_STRING over a STRING-typed column yields a JSON string literal,
// so every row would be skipped and the read would answer an empty window
// with no error at all: a measured zero on a money leg.
func TestWrongPayloadColumnIsLoud(t *testing.T) {
	ev := outcome("invoice.pay", "capture", "failed", "inv_1", "h:c1", "smb", "USD", 14900, from.Add(time.Minute))
	cases := []struct {
		name    string
		rows    []fakeRow
		wantErr string
		want    int64 // expected total SumMinor when no error is expected
	}{
		{
			// Every payload double-encoded, as a STRING column would be.
			name: "no row is a JSON object",
			rows: []fakeRow{
				{micros: from.Add(time.Minute).UnixMicro(), payload: `"{\"event\":\"biz.outcome\"}"`},
				{micros: from.Add(2 * time.Minute).UnixMicro(), payload: `"plain text log line"`},
			},
			wantErr: "not a JSON object",
		},
		{
			name:    "a JSON array is not an object either",
			rows:    []fakeRow{{micros: from.Add(time.Minute).UnixMicro(), payload: `[1,2,3]`}},
			wantErr: "not a JSON object",
		},
		{
			name:    "a JSON null is not an object either",
			rows:    []fakeRow{{micros: from.Add(time.Minute).UnixMicro(), payload: `null`}},
			wantErr: "not a JSON object",
		},
		{
			// One real object among the noise means the column IS
			// json_payload and the rest are other services' log lines.
			name: "one object among non-objects is a normal mixed log stream",
			rows: []fakeRow{
				{micros: from.Add(time.Minute).UnixMicro(), payload: `plain text log line`},
				{micros: from.Add(2 * time.Minute).UnixMicro(), payload: eventRecord(ev)},
			},
			want: 14900,
		},
		{
			// An empty window is an empty window, not a column error.
			name: "no rows at all is not a column error",
			rows: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newFakeBigQuery(t, c.rows)
			srv.evaluate = false // the rows reach the decoder as-is
			groups, err := srv.querier(t).QueryEvents(context.Background(), query.EventQuery{
				Range: window(), Filters: map[string]string{"currency": "USD"},
			})
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				if groups != nil {
					t.Fatalf("groups = %+v, want nil alongside the error", groups)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var sum int64
			for _, g := range groups {
				sum += g.SumMinor
			}
			if sum != c.want {
				t.Fatalf("sum = %d, want %d", sum, c.want)
			}
		})
	}
}
