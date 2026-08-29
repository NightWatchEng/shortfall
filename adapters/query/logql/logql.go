// Package logql is an events-only query.Querier backed by Grafana Loki: it
// fetches the biz.* outcome lines the loki exporter pushed and delegates
// aggregation to the in-memory reference (query/memq), so its numbers agree
// with memq by construction — the only surface that can diverge is the
// fetch and the line parse, which the golden harness verifies live.
//
// Loki is a log store, not a metrics store, so Capabilities declares
// Metrics=false: the engine grounds realized and customers here and reads
// the metric legs from a metrics backend.
//
// Nested module, stdlib only: Loki's HTTP API is JSON.
package logql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/eventline"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
)

// Doer is the slice of *http.Client this adapter needs (a test seam).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Querier reads outcome events back from a Loki base URL.
type Querier struct {
	base      string
	doer      Doer
	orgID     string
	pageLimit int
}

var _ query.Querier = (*Querier)(nil)

// Option configures the Querier.
type Option func(*Querier)

// WithHTTPClient injects the HTTP doer (default 30s *http.Client).
func WithHTTPClient(d Doer) Option { return func(q *Querier) { q.doer = d } }

// WithOrgID sets Loki's multi-tenancy header (X-Scope-OrgID).
func WithOrgID(id string) Option { return func(q *Querier) { q.orgID = id } }

// WithPageLimit sets the per-request entry limit (default 1000; Loki's
// server-side cap is typically 5000).
func WithPageLimit(n int) Option { return func(q *Querier) { q.pageLimit = n } }

// New builds a Querier for a Loki base URL, e.g. "http://loki:3100".
func New(baseURL string, opts ...Option) *Querier {
	q := &Querier{
		base:      strings.TrimRight(baseURL, "/"),
		doer:      &http.Client{Timeout: 30 * time.Second},
		pageLimit: 1000,
	}
	for _, o := range opts {
		o(q)
	}
	return q
}

// Capabilities: events only.
func (q *Querier) Capabilities() query.Caps {
	return query.Caps{Events: true, Metrics: false}
}

// QueryMetric is unsupported: Loki stores logs, not the biz_* families.
func (q *Querier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, query.ErrUnsupported
}

// QueryEvents fetches the window's outcome lines (stream-label filters
// pushed down, the rest applied by the reference) and aggregates them
// exactly as memq would.
func (q *Querier) QueryEvents(ctx context.Context, qy query.EventQuery) (query.EventGroups, error) {
	events, err := q.fetch(ctx, qy)
	if err != nil {
		return nil, err
	}
	return memq.New(memq.WithEvents(events)).QueryEvents(ctx, qy)
}

// selector builds the LogQL stream selector: flow/stage/outcome are the
// exporter's stream labels, so equality filters push down; everything else
// (currency, customer, segment, entity) lives in the line and is filtered
// after the parse by the reference. With no pushdown, outcome=~".+" selects
// every shortfall stream (the exporter always sets it).
func selector(filters map[string]string) string {
	parts := []string{}
	for _, k := range []string{"flow", "stage", "outcome"} {
		if v, ok := filters[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, `outcome=~".+"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// fetch pages /loki/api/v1/query_range forward over [From, To). Pagination
// advances past the last returned timestamp; entries sharing that exact
// nanosecond with the page boundary would be skipped — outcome events carry
// distinct times in practice, and the golden harness would catch a loss.
func (q *Querier) fetch(ctx context.Context, qy query.EventQuery) ([]biz.Outcome, error) {
	var out []biz.Outcome
	start := qy.Range.From.UnixNano()
	end := qy.Range.To.UnixNano()
	for {
		v := url.Values{}
		v.Set("query", selector(qy.Filters))
		v.Set("start", strconv.FormatInt(start, 10))
		v.Set("end", strconv.FormatInt(end, 10))
		v.Set("limit", strconv.Itoa(q.pageLimit))
		v.Set("direction", "forward")
		body, err := q.get(ctx, "/loki/api/v1/query_range", v)
		if err != nil {
			return nil, err
		}
		entries, lastTS, err := parseStreams(body)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
		if len(entries) < q.pageLimit || lastTS >= end-1 {
			return out, nil
		}
		start = lastTS + 1
	}
}

func (q *Querier) get(ctx context.Context, path string, v url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.base+path+"?"+v.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("logql: request: %w", err)
	}
	if q.orgID != "" {
		req.Header.Set("X-Scope-OrgID", q.orgID)
	}
	resp, err := q.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("logql: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("logql: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logql: status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// parseStreams decodes a query_range streams response into outcomes stamped
// with each entry's Loki timestamp, returning the latest timestamp seen. A
// line that is not a biz outcome fails loudly: the selector targets the
// exporter's streams, so a foreign line means a misconfigured target and
// silently skipping it could undercount money.
func parseStreams(body []byte) ([]biz.Outcome, int64, error) {
	var env struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, 0, fmt.Errorf("logql: decode: %w", err)
	}
	if env.Status != "success" {
		return nil, 0, fmt.Errorf("logql: query status %q", env.Status)
	}
	var out []biz.Outcome
	var last int64
	for _, s := range env.Data.Result {
		for _, v := range s.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("logql: entry timestamp %q: %w", v[0], err)
			}
			o, err := eventline.Parse([]byte(v[1]), time.Unix(0, ns).UTC())
			if err != nil {
				return nil, 0, err
			}
			out = append(out, o)
			if ns > last {
				last = ns
			}
		}
	}
	return out, last, nil
}
