// Package gcplogging is an events-only query.Querier backed by Cloud
// Logging: it reads back the structured outcome entries the
// adapters/export/gcp exporter writes, keeps only the entries marked
// event="biz.outcome" (a log bucket carries everything the project logs —
// the marker is what tells outcomes apart), parses each biz.* payload with
// the shared eventline decoder, and delegates aggregation to the in-memory
// reference (query/memq), so its numbers agree with memq by construction.
//
// # Which read path, and why
//
// A log bucket upgraded to Log Analytics is queried with SQL, and the
// documented programmatic route to that SQL is the bucket's linked BigQuery
// dataset: one REST call to BigQuery's jobs.query. That is the path this
// adapter takes, over the `_AllLogs` view.
//
// The alternative — routing a sink into a plain BigQuery table — was
// considered and rejected rather than offered behind an option. A sink
// sanitizes payload field names on the way in, so `biz.flow` lands as
// `biz_flow`; the exporter's record would need a second decoder that
// disagreed with the first about what the wire convention is, which is the
// documentation-and-code drift ADR-0008 exists to prevent. Log Analytics
// keeps the payload verbatim in a JSON column, so the same eventline decoder
// reads a Cloud Logging entry and a CloudWatch EMF record. One convention,
// one decoder. Naming a different view or table in the linked dataset is
// supported (WithView) because that changes no field name.
//
// # Where the correctness boundary sits
//
// The window, the outcome marker and the query's equality filters are pushed
// into the SQL as bound NAMED parameters — never string-interpolated — but
// they are an efficiency for the backend, not the correctness boundary:
// memq re-applies the exact half-open [From, To) and the same filters over
// the decoded events. Aggregation is not pushed down. That is a deliberate
// trade with a real cost: the adapter fetches the window's matching entries
// rather than letting BigQuery reduce them, so EventAggDistinctCount reads
// every matching row instead of counting server-side. It buys parity with
// memq by construction on the money paths, which is what the realized and
// customer-impact legs are read from. WithMaxRows caps the fetch, and
// reaching the cap is a loud error — a silently truncated window would
// understate money.
//
// # Capabilities
//
// Events only. Cloud Logging is not a metric store, and the GCP metric legs
// (deferred, unrealized, the baseline) are read from Managed Service for
// Prometheus through adapters/query/promql. QueryMetric returns
// query.ErrUnsupported so the engine reports an honest NotAvailable with a
// reason instead of a confident zero.
//
// Nested module, standard library only: BigQuery's jobs.query is JSON over
// HTTPS, and credentials arrive either through the injected HTTP client (an
// oauth2 transport, say) or through WithBearerToken — so no cloud SDK is
// pulled in, and nothing here needs a live GCP project to test.
package gcplogging

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/eventline"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
)

// eventMarker is the payload value adapters/export/gcp stamps on every
// outcome entry (its `event` key). Entries without it are not ours.
const eventMarker = biz.EventOutcome

// defaultView is the view a Log Analytics linked dataset exposes over a
// log bucket's entries.
const defaultView = "_AllLogs"

// defaultMaxRows caps one window's fetch. A window that needs more entries
// than this is not silently truncated: it is an error naming the cap.
const defaultMaxRows = 100000

// payloadPaths maps a query label to the JSONPath of the payload key that
// carries it. It is the allowlist as well as the mapping: a label that is
// not here is refused, which is also what keeps every identifier in the
// generated SQL a constant of this package. Values are always bound as
// parameters, never rendered into the statement.
var payloadPaths = map[string]string{
	"flow":     jsonPath(biz.AttrFlow),
	"stage":    jsonPath(biz.AttrStage),
	"outcome":  jsonPath(biz.AttrOutcome),
	"currency": jsonPath(biz.AttrCurrency),
	"segment":  jsonPath(biz.AttrSegment),
	"kind":     jsonPath(biz.AttrValueKind),
	"customer": jsonPath(biz.AttrCustomerID),
	"entity":   jsonPath(biz.AttrEntityID),
}

// jsonPath quotes an attribute name as a JSONPath selector. The names come
// from biz rather than being spelled here, so the read side cannot drift
// from what the exporters write — which is how three surfaces ended up with
// three spellings of the same facts (workspace-cnz). The quoting is what
// lets a dotted name like biz.entity.id select one key rather than three
// levels of nesting.
func jsonPath(attr string) string { return `$."` + attr + `"` }

// Doer is the slice of *http.Client this adapter needs (a test seam).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// TokenSource returns a bearer token for the BigQuery API. It is called
// once per request so a short-lived token can be refreshed.
type TokenSource func() (string, error)

// Querier reads outcome events back out of a Log Analytics log bucket
// through its linked BigQuery dataset.
type Querier struct {
	projectID string
	dataset   string
	view      string
	location  string

	endpoint     string
	doer         Doer
	token        TokenSource
	pollInterval time.Duration
	maxRows      int

	eventHistWeeks int
}

var _ query.Querier = (*Querier)(nil)

// Option configures the Querier.
type Option func(*Querier)

// WithHTTPClient injects the HTTP doer (default: an *http.Client at
// defaultHTTPTimeout). Bring an oauth2 client here to authenticate without
// WithBearerToken.
func WithHTTPClient(d Doer) Option { return func(q *Querier) { q.doer = d } }

// WithEndpoint overrides the API endpoint (private endpoints, tests);
// default https://bigquery.googleapis.com.
func WithEndpoint(u string) Option {
	return func(q *Querier) { q.endpoint = strings.TrimRight(u, "/") }
}

// WithBearerToken sets the source of the Authorization bearer token. With
// no token source the adapter sends no Authorization header and the
// injected HTTP client is expected to authenticate.
func WithBearerToken(ts TokenSource) Option { return func(q *Querier) { q.token = ts } }

// WithView names the view or table in the linked dataset (default
// "_AllLogs", the view Log Analytics exposes over a log bucket).
func WithView(v string) Option { return func(q *Querier) { q.view = v } }

// WithLocation sets the BigQuery job location. A linked dataset lives in
// the log bucket's region, and a query without a matching location fails to
// find the dataset.
func WithLocation(l string) Option { return func(q *Querier) { q.location = l } }

// WithPollInterval sets the cadence for polling a query job that did not
// complete inline (default 250ms). It must be positive: a zero or negative
// interval turns the poll into an unthrottled loop against a billed API,
// so New refuses it rather than letting it through as "no delay".
func WithPollInterval(d time.Duration) Option { return func(q *Querier) { q.pollInterval = d } }

// WithMaxRows caps how many entries one window may fetch (default 100000).
// Reaching the cap is an error, never a smaller answer.
func WithMaxRows(n int) Option { return func(q *Querier) { q.maxRows = n } }

// WithEventHistoryWeeks declares the log bucket's retention, in weeks, for
// Caps. It defaults to 0 — unknown — because retention is a property of the
// bucket, not of this code, and claiming a window the bucket cannot serve
// would have the engine ground a leg on data that is not there.
func WithEventHistoryWeeks(w int) Option { return func(q *Querier) { q.eventHistWeeks = w } }

// New builds a Querier over the linked BigQuery dataset of a Log
// Analytics-upgraded log bucket. projectID is the project billed for the
// query and holding the dataset; dataset is the linked dataset's id.
//
// The three identifiers are validated here, at construction, because they
// are the only parts of the generated SQL that are not constants of this
// package.
func New(projectID, dataset string, opts ...Option) (*Querier, error) {
	q := &Querier{
		projectID:    projectID,
		dataset:      dataset,
		view:         defaultView,
		endpoint:     "https://bigquery.googleapis.com",
		doer:         &http.Client{Timeout: defaultHTTPTimeout},
		pollInterval: 250 * time.Millisecond,
		maxRows:      defaultMaxRows,
	}
	for _, o := range opts {
		o(q)
	}
	if !validProjectID(q.projectID) {
		return nil, fmt.Errorf("gcplogging: invalid project id %q", q.projectID)
	}
	if !validBigQueryID(q.dataset) {
		return nil, fmt.Errorf("gcplogging: invalid dataset id %q", q.dataset)
	}
	if !validBigQueryID(q.view) {
		return nil, fmt.Errorf("gcplogging: invalid view id %q", q.view)
	}
	if q.maxRows <= 0 {
		return nil, fmt.Errorf("gcplogging: max rows must be positive, got %d", q.maxRows)
	}
	if q.pollInterval <= 0 {
		return nil, fmt.Errorf("gcplogging: poll interval must be positive, got %s", q.pollInterval)
	}
	return q, nil
}

// Capabilities: events only. The metric legs come from Managed Service for
// Prometheus through adapters/query/promql.
func (q *Querier) Capabilities() query.Caps {
	return query.Caps{Metrics: false, Events: true, EventHistoryWeeks: q.eventHistWeeks}
}

// QueryMetric is unsupported: Cloud Logging stores entries, not series, and
// extracts no metrics from them the way CloudWatch EMF does.
func (q *Querier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, query.ErrUnsupported
}

// QueryEvents runs one Log Analytics read over the window and aggregates
// the decoded outcomes exactly as memq would — including the currency
// invariant (ADR-0001), the group ordering, and the Limit contract.
func (q *Querier) QueryEvents(ctx context.Context, qy query.EventQuery) (query.EventGroups, error) {
	events, err := q.fetch(ctx, qy)
	if err != nil {
		return nil, err
	}
	return memq.New(memq.WithEvents(events)).QueryEvents(ctx, qy)
}

// fetch runs the statement and decodes every marked entry it returns, in
// the store's timestamp order.
func (q *Querier) fetch(ctx context.Context, qy query.EventQuery) ([]biz.Outcome, error) {
	if err := checkLabels(qy); err != nil {
		return nil, err
	}
	stmt, params := q.statement(qy)
	rows, err := q.run(ctx, stmt, params)
	if err != nil {
		return nil, err
	}
	// The statement asks for maxRows+1 so a full page is distinguishable
	// from a window that simply fit: more than the cap means BigQuery cut
	// the window off and every aggregate below it would understate money.
	if len(rows) > q.maxRows {
		return nil, fmt.Errorf("gcplogging: the window returned more than the %d-row cap — it is truncated; narrow the window or raise WithMaxRows", q.maxRows)
	}
	return decodeRows(rows)
}

// checkLabels refuses a filter or group label outside the allowlist. An
// unrecognised label would project to the empty string and quietly answer a
// different question than the one asked.
func checkLabels(qy query.EventQuery) error {
	for k := range qy.Filters {
		if _, ok := payloadPaths[k]; !ok {
			return fmt.Errorf("gcplogging: unknown filter label %q", k)
		}
	}
	for _, g := range qy.GroupBy {
		if _, ok := payloadPaths[g]; !ok {
			return fmt.Errorf("gcplogging: unknown group label %q", g)
		}
	}
	return nil
}

// statement renders the one reviewed read and its bound parameters.
//
// Every value is a NAMED parameter. The only interpolated pieces are the
// table reference (three identifiers validated in New against a character
// set that cannot close a backtick or open a string) and the row cap (an
// int), so no caller-supplied text ever reaches the SQL text.
func (q *Querier) statement(qy query.EventQuery) (string, []queryParam) {
	params := []queryParam{
		lowerBoundParam("window_from", qy.Range.From),
		upperBoundParam("window_to", qy.Range.To),
		stringParam("event_marker", eventMarker),
	}
	conds := []string{
		"timestamp >= @window_from AND timestamp < @window_to",
		"JSON_VALUE(json_payload, '$.event') = @event_marker",
	}
	// Sorted, so the statement (and any BigQuery query cache hit) is
	// deterministic for a given query.
	keys := make([]string, 0, len(qy.Filters))
	for k := range qy.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := "f_" + k
		// IFNULL, because the exporter omits an empty biz.segment entirely
		// and memq projects a missing label to "" — without it a filter on
		// segment="" would match nothing where the reference matches every
		// unsegmented event.
		conds = append(conds, fmt.Sprintf("IFNULL(JSON_VALUE(json_payload, '%s'), '') = @%s", payloadPaths[k], name))
		params = append(params, stringParam(name, qy.Filters[k]))
	}

	stmt := "SELECT UNIX_MICROS(timestamp) AS event_micros, TO_JSON_STRING(json_payload) AS payload" +
		" FROM `" + q.projectID + "." + q.dataset + "." + q.view + "`" +
		" WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY timestamp" +
		" LIMIT " + strconv.Itoa(q.maxRows+1)
	return stmt, params
}

// decodeRows turns (event_micros, payload) rows into outcomes, preserving
// the store's order.
//
// Three cases, and the difference between them is where money can go
// missing:
//
//   - A payload that is a JSON object WITHOUT the outcome marker is
//     skipped. A log bucket's view carries every log the project writes,
//     and refusing to read a window because an unrelated service logged
//     JSON would make the adapter useless.
//   - A payload that is not a JSON object at all is also skipped — a plain
//     text log line is not ours either — but it is counted, because a
//     result set in which NOTHING decoded as an object is not a quiet
//     window, it is the wrong column: TO_JSON_STRING over a STRING-typed
//     column yields a JSON string literal, not an object, so every row
//     would be dropped and the read would answer an empty window with no
//     error at all. That shape is refused below.
//   - A payload that IS a marked object and still fails to parse is a loud
//     error — a truncated or corrupted outcome record, and counting it as
//     nothing would understate money.
func decodeRows(rows []bqRow) ([]biz.Outcome, error) {
	out := make([]biz.Outcome, 0, len(rows))
	nonObjects := 0
	for i, r := range rows {
		if len(r.F) != 2 {
			return nil, fmt.Errorf("gcplogging: row %d has %d columns, want 2 (event_micros, payload)", i, len(r.F))
		}
		micros, err := strconv.ParseInt(r.F[0].V, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("gcplogging: row %d: unparsable event_micros %q: %w", i, r.F[0].V, err)
		}
		payload := []byte(r.F[1].V)
		marked, isObject := outcomeMarker(payload)
		if !isObject {
			nonObjects++
			continue
		}
		if !marked {
			continue
		}
		o, err := eventline.Parse(payload, time.UnixMicro(micros).UTC())
		if err != nil {
			return nil, fmt.Errorf("gcplogging: row %d: %w", i, err)
		}
		out = append(out, o)
	}
	// Every row came back, and not one of them was a JSON object: the
	// payload column is not the JSON column this adapter reads. Answering
	// "no outcomes" there would be a measured zero on a money leg.
	if len(rows) > 0 && nonObjects == len(rows) {
		return nil, fmt.Errorf("gcplogging: all %d rows carry a payload that is not a JSON object — "+
			"the view's payload column is not Log Analytics' json_payload; check WithView", len(rows))
	}
	return out, nil
}

// outcomeMarker reports whether a payload is a JSON object, and if so
// whether it carries the exporter's marker. The two answers are separate
// because "not ours" and "not the column we asked for" need different
// handling: the first is skipped, the second is refused by the caller.
func outcomeMarker(payload []byte) (marked, isObject bool) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil || probe == nil {
		return false, false
	}
	raw, ok := probe["event"]
	if !ok {
		return false, true
	}
	var event string
	if err := json.Unmarshal(raw, &event); err != nil {
		return false, true
	}
	return event == eventMarker, true
}

// validProjectID accepts letters in either case, digits, the hyphen and
// underscore, and the dot and colon a legacy domain-scoped id carries. That
// is wider than Google's own project-id rules, and deliberately so: the
// point is not to reproduce those rules but that none of these characters
// can close the backtick quoting the table reference sits in, or open a
// string or comment. The set below is the whole of what is admitted, so
// the safety argument quantifies over all of it.
func validProjectID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == ':', r == '_':
		default:
			return false
		}
	}
	return true
}

// validBigQueryID accepts a bare BigQuery dataset/table id: letters, digits
// and underscores, which is exactly what BigQuery allows unquoted.
func validBigQueryID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}
