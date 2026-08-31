// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package gcplogging

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeRow is one (event_micros, payload) row of a Log Analytics read.
type fakeRow struct {
	micros  int64
	payload string
}

// fakeBigQuery stands in for the BigQuery jobs.query REST surface over a
// Log Analytics linked dataset.
//
// It models two things, and the difference between them is the point.
//
// The wire envelope is faithful: the jobComplete=false poll, paging by
// pageToken, the string-encoded INT64 column, the error object.
//
// The SQL is EVALUATED, not ignored — by a deliberately small interpreter
// (planStatement below) that understands exactly the statement shape this
// adapter emits and nothing else. That matters because the pushed-down
// predicates are otherwise the one part of the adapter no parity fence can
// reach: a fake that served every row regardless would leave all eight
// payloadPaths JSONPaths inert, and a drifted path would return zero rows
// against real BigQuery while every test stayed green — a silent zero on
// the realized leg, which the engine reports as a deterministic
// measurement. A clause the interpreter does not recognise is a loud test
// failure, never a silently skipped predicate.
//
// `evaluate: false` models the opposite backend: one that ignores the
// pushed-down predicates entirely (LocalStack does exactly this to a Logs
// Insights filter clause). The adapter must still answer correctly there,
// because memq re-applies the window and the filters and the decoder skips
// unmarked entries. Both modes are run.
type fakeBigQuery struct {
	t    *testing.T
	srv  *httptest.Server
	rows []fakeRow

	evaluate   bool // apply the statement's predicates (default true)
	pageSize   int
	stallOnce  bool // answer the first insert with jobComplete:false
	stalled    bool
	failStatus int
	failBody   string
	// repeatToken makes every page hand back the SAME pageToken — the
	// runaway an API bug would produce, which the adapter must refuse
	// rather than loop on.
	repeatToken bool
	// incompleteOnPage makes the getQueryResults for a later page answer
	// jobComplete:false; those rows would be silently missing.
	incompleteOnPage bool

	queries       int
	lastQuery     string
	lastParams    map[string]string
	lastAuth      string
	lastBody      map[string]any
	pollLocations []string  // the ?location= on each getQueryResults
	served        []fakeRow // the rows the current query resolved to
}

func newFakeBigQuery(t *testing.T, rows []fakeRow) *fakeBigQuery {
	t.Helper()
	f := &fakeBigQuery{t: t, rows: rows, pageSize: 1000, evaluate: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// querier builds an adapter pointed at the fake, with a bearer token so the
// Authorization header is exercised.
func (f *fakeBigQuery) querier(t *testing.T, opts ...Option) *Querier {
	t.Helper()
	base := []Option{
		WithEndpoint(f.srv.URL),
		WithHTTPClient(f.srv.Client()),
		WithLocation("us"),
		WithBearerToken(func() (string, error) { return "ya29.test", nil }),
		WithPollInterval(time.Millisecond),
	}
	q, err := New("my-project", "logs_analytics", append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return q
}

func (f *fakeBigQuery) handle(w http.ResponseWriter, r *http.Request) {
	f.lastAuth = r.Header.Get("Authorization")
	if f.failStatus != 0 {
		w.WriteHeader(f.failStatus)
		_, _ = io.WriteString(w, f.failBody)
		return
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/queries"):
		f.handleInsert(w, r)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/queries/"):
		f.handleGetResults(w, r)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeBigQuery) handleInsert(w http.ResponseWriter, r *http.Request) {
	if want := "/bigquery/v2/projects/my-project/queries"; r.URL.Path != want {
		f.t.Errorf("insert path = %q, want %q", r.URL.Path, want)
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("insert body: %v", err)
	}
	f.queries++
	f.lastBody = body
	f.lastQuery, _ = body["query"].(string)
	f.lastParams = map[string]string{}
	if ps, ok := body["queryParameters"].([]any); ok {
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			name, _ := pm["name"].(string)
			val, _ := pm["parameterValue"].(map[string]any)
			s, _ := val["value"].(string)
			f.lastParams["@"+name] = s
		}
	}
	f.served = f.resolve()

	if f.stallOnce && !f.stalled {
		f.stalled = true
		f.writeJSON(w, map[string]any{
			"jobComplete":  false,
			"jobReference": map[string]any{"jobId": "job-1", "location": "us"},
		})
		return
	}
	f.writePage(w, 0)
}

func (f *fakeBigQuery) handleGetResults(w http.ResponseWriter, r *http.Request) {
	f.pollLocations = append(f.pollLocations, r.URL.Query().Get("location"))
	from := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			f.t.Errorf("bad pageToken %q", tok)
		}
		from = n
		if f.incompleteOnPage {
			f.writeJSON(w, map[string]any{
				"jobComplete":  false,
				"jobReference": map[string]any{"jobId": "job-1", "location": "us"},
			})
			return
		}
	}
	f.writePage(w, from)
}

// --- the statement interpreter ----------------------------------------

// payloadPredicate is one pushed-down payload equality: the JSON key the
// statement extracts, and the value it was compared against.
type payloadPredicate struct {
	key   string
	value string
}

// stmtPlan is what the interpreter recovers from one statement.
//
// filters is a SLICE, not a map keyed by JSON key, and that is load-bearing.
// Two query labels can resolve to the same JSONPath — that is exactly what a
// drifted payloadPaths entry looks like — and the statement then carries two
// contradictory equalities on one key, which real BigQuery answers with zero
// rows. A map would keep only the last of them and quietly serve the rows the
// dropped predicate should have excluded: a predicate this harness would be
// pretending to check.
type stmtPlan struct {
	table   string
	from    time.Time // @window_from, inclusive
	to      time.Time // @window_to, exclusive
	marker  string    // required value of the payload's "event" key
	filters []payloadPredicate
	limit   int
}

var (
	stmtRE = regexp.MustCompile(
		"^SELECT UNIX_MICROS\\(timestamp\\) AS event_micros, TO_JSON_STRING\\(json_payload\\) AS payload" +
			" FROM `([^`]+)` WHERE (.+) ORDER BY timestamp LIMIT ([0-9]+)$")
	markerRE = regexp.MustCompile(`^JSON_VALUE\(json_payload, '\$\.event'\) = (@\w+)$`)
	filterRE = regexp.MustCompile(`^IFNULL\(JSON_VALUE\(json_payload, '\$\."([^"]+)"'\), ''\) = (@\w+)$`)
	fromRE   = regexp.MustCompile(`^timestamp >= (@\w+)$`)
	toRE     = regexp.MustCompile(`^timestamp < (@\w+)$`)
)

// planStatement parses the statement into a plan, failing the test loudly
// on anything it does not recognise — an unrecognised clause silently
// dropped would be a predicate this harness pretends to check.
func (f *fakeBigQuery) planStatement() (stmtPlan, bool) {
	f.t.Helper()
	m := stmtRE.FindStringSubmatch(f.lastQuery)
	if m == nil {
		f.t.Errorf("fake cannot parse the statement — extend the interpreter or the adapter drifted:\n%s", f.lastQuery)
		return stmtPlan{}, false
	}
	plan := stmtPlan{table: m[1]}
	limit, err := strconv.Atoi(m[3])
	if err != nil {
		f.t.Errorf("bad LIMIT %q", m[3])
		return stmtPlan{}, false
	}
	plan.limit = limit

	ok := true
	param := func(name string) string {
		v, present := f.lastParams[name]
		if !present {
			f.t.Errorf("statement references %s but no such parameter was bound", name)
			ok = false
		}
		return v
	}
	ts := func(name string) time.Time {
		t, err := time.Parse(bqTimestampLayout, param(name))
		if err != nil {
			f.t.Errorf("parameter %s = %q is not a BigQuery TIMESTAMP literal: %v", name, f.lastParams[name], err)
			ok = false
		}
		return t
	}
	for _, clause := range strings.Split(m[2], " AND ") {
		clause = strings.TrimSpace(clause)
		switch {
		case fromRE.MatchString(clause):
			plan.from = ts(fromRE.FindStringSubmatch(clause)[1])
		case toRE.MatchString(clause):
			plan.to = ts(toRE.FindStringSubmatch(clause)[1])
		case markerRE.MatchString(clause):
			plan.marker = param(markerRE.FindStringSubmatch(clause)[1])
		case filterRE.MatchString(clause):
			g := filterRE.FindStringSubmatch(clause)
			plan.filters = append(plan.filters, payloadPredicate{key: g[1], value: param(g[2])})
		default:
			f.t.Errorf("fake cannot evaluate WHERE clause %q — it would be silently ignored", clause)
			ok = false
		}
	}
	if plan.from.IsZero() || plan.to.IsZero() {
		f.t.Errorf("statement bound no window: %s", f.lastQuery)
		ok = false
	}
	// Two labels on one JSONPath is a drifted payloadPaths entry, not a
	// legitimate statement: the adapter has emitted two contradictory
	// equalities on one key, which real BigQuery answers with zero rows.
	// Name it here rather than letting the evaluation below merely return
	// nothing.
	seen := map[string]string{}
	for _, p := range plan.filters {
		if prev, dup := seen[p.key]; dup && prev != p.value {
			f.t.Errorf("statement filters key %q against both %q and %q — two labels share one JSONPath, "+
				"which real BigQuery answers with zero rows:\n%s", p.key, prev, p.value, f.lastQuery)
			ok = false
		}
		seen[p.key] = p.value
	}
	return plan, ok
}

// resolve applies the parsed statement to the fixture rows.
func (f *fakeBigQuery) resolve() []fakeRow {
	f.t.Helper()
	plan, ok := f.planStatement()
	if !ok {
		return nil
	}
	// The view varies (WithView), the project and dataset do not.
	if !strings.HasPrefix(plan.table, "my-project.logs_analytics.") {
		f.t.Errorf("statement table = %q, want the my-project.logs_analytics dataset", plan.table)
	}

	out := make([]fakeRow, 0, len(f.rows))
	for _, row := range f.rows {
		if f.evaluate && !matchesPlan(row, plan) {
			continue
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].micros < out[j].micros })
	if plan.limit > 0 && len(out) > plan.limit {
		out = out[:plan.limit]
	}
	return out
}

// matchesPlan is the row-level evaluation: the half-open window on the
// entry timestamp, the outcome marker, and each pushed-down payload
// equality (a missing key reads as "", matching the adapter's IFNULL).
func matchesPlan(row fakeRow, plan stmtPlan) bool {
	at := time.UnixMicro(row.micros).UTC()
	if at.Before(plan.from) || !at.Before(plan.to) {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(row.payload), &payload); err != nil {
		// A non-JSON line has no json_payload at all: every JSON_VALUE
		// predicate over it is NULL, so it matches nothing.
		return false
	}
	if plan.marker != "" && jsonValue(payload, "event") != plan.marker {
		return false
	}
	for _, p := range plan.filters {
		if jsonValue(payload, p.key) != p.value {
			return false
		}
	}
	return true
}

// jsonValue is BigQuery's JSON_VALUE over a top-level key, rendered the way
// JSON_VALUE renders a scalar (a missing key is the empty string, matching
// the adapter's IFNULL(..., ”)).
func jsonValue(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// --- response rendering ------------------------------------------------

func (f *fakeBigQuery) writePage(w http.ResponseWriter, from int) {
	end := from + f.pageSize
	if end > len(f.served) {
		end = len(f.served)
	}
	rows := make([]any, 0, end-from)
	for _, row := range f.served[from:end] {
		rows = append(rows, map[string]any{"f": []any{
			map[string]any{"v": strconv.FormatInt(row.micros, 10)},
			map[string]any{"v": row.payload},
		}})
	}
	resp := map[string]any{
		"kind":         "bigquery#queryResponse",
		"jobComplete":  true,
		"totalRows":    strconv.Itoa(len(f.served)),
		"jobReference": map[string]any{"jobId": "job-1", "location": "us"},
		"schema": map[string]any{"fields": []any{
			map[string]any{"name": "event_micros", "type": "INTEGER"},
			map[string]any{"name": "payload", "type": "STRING"},
		}},
		"rows": rows,
	}
	if end < len(f.served) {
		if f.repeatToken {
			resp["pageToken"] = strconv.Itoa(from)
		} else {
			resp["pageToken"] = strconv.Itoa(end)
		}
	}
	f.writeJSON(w, resp)
}

func (f *fakeBigQuery) writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("marshal: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(b); err != nil {
		f.t.Errorf("write: %v", err)
	}
}

// rawQuerier points an adapter at a server that answers every request with
// one fixed body — for pinning what the decoder does with a malformed
// response.
func rawQuerier(t *testing.T, body string) *Querier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	q, err := New("my-project", "logs_analytics",
		WithEndpoint(srv.URL), WithHTTPClient(srv.Client()), WithPollInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return q
}

// errorBody renders the BigQuery error envelope.
func errorBody(code int, msg string) string {
	return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"status":"INVALID_ARGUMENT"}}`, code, msg)
}
