package gcplogging

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
// Log Analytics linked dataset. It does NOT evaluate SQL: like LocalStack
// ignoring an Insights filter clause, it serves every fixture row and
// leaves the window and filter arithmetic to the adapter's reference
// aggregation — which is exactly where this adapter puts the correctness
// boundary. What it does model faithfully is the wire envelope: the
// jobComplete=false poll, paging by pageToken, the string-encoded INT64
// column, and the error object.
type fakeBigQuery struct {
	t    *testing.T
	srv  *httptest.Server
	rows []fakeRow

	pageSize   int
	stallOnce  bool // answer the first insert with jobComplete:false
	stalled    bool
	failStatus int
	failBody   string

	queries    int
	lastQuery  string
	lastParams map[string]string
	lastAuth   string
	lastBody   map[string]any
}

func newFakeBigQuery(t *testing.T, rows []fakeRow) *fakeBigQuery {
	t.Helper()
	f := &fakeBigQuery{t: t, rows: rows, pageSize: 1000}
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
	from := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			f.t.Errorf("bad pageToken %q", tok)
		}
		from = n
	}
	f.writePage(w, from)
}

func (f *fakeBigQuery) writePage(w http.ResponseWriter, from int) {
	end := from + f.pageSize
	if end > len(f.rows) {
		end = len(f.rows)
	}
	rows := make([]any, 0, end-from)
	for _, row := range f.rows[from:end] {
		rows = append(rows, map[string]any{"f": []any{
			map[string]any{"v": strconv.FormatInt(row.micros, 10)},
			map[string]any{"v": row.payload},
		}})
	}
	resp := map[string]any{
		"kind":         "bigquery#queryResponse",
		"jobComplete":  true,
		"totalRows":    strconv.Itoa(len(f.rows)),
		"jobReference": map[string]any{"jobId": "job-1", "location": "us"},
		"schema": map[string]any{"fields": []any{
			map[string]any{"name": "event_micros", "type": "INTEGER"},
			map[string]any{"name": "payload", "type": "STRING"},
		}},
		"rows": rows,
	}
	if end < len(f.rows) {
		resp["pageToken"] = strconv.Itoa(end)
	}
	f.writeJSON(w, resp)
}

func (f *fakeBigQuery) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("marshal: %v", err)
		return
	}
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
