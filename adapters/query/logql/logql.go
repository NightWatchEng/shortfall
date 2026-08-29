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

// fetch pages /loki/api/v1/query_range forward over [From, To). A page can
// be cut mid-nanosecond (Loki's limit is global across streams), so the
// next page re-requests from the last timestamp seen and de-duplicates the
// entries already taken there; the per-request limit grows by the carry so
// the boundary entries can never starve progress. A single nanosecond
// holding more entries than the server's limit cap fails loudly rather
// than silently dropping money.
func (q *Querier) fetch(ctx context.Context, qy query.EventQuery) ([]biz.Outcome, error) {
	var out []biz.Outcome
	start := qy.Range.From.UnixNano()
	end := qy.Range.To.UnixNano()
	seen := map[string]struct{}{} // entries already taken at the current start ns
	for {
		limit := q.pageLimit + len(seen)
		v := url.Values{}
		v.Set("query", selector(qy.Filters))
		v.Set("start", strconv.FormatInt(start, 10))
		v.Set("end", strconv.FormatInt(end, 10))
		v.Set("limit", strconv.Itoa(limit))
		v.Set("direction", "forward")
		body, err := q.get(ctx, "/loki/api/v1/query_range", v)
		if err != nil {
			return nil, err
		}
		entries, err := parseStreams(body)
		if err != nil {
			return nil, err
		}
		fresh := 0
		var maxTS int64
		for _, e := range entries {
			if e.ns > maxTS {
				maxTS = e.ns
			}
			if e.ns == start {
				if _, dup := seen[e.key()]; dup {
					continue
				}
			}
			out = append(out, e.outcome)
			fresh++
		}
		if len(entries) < limit {
			return out, nil
		}
		switch {
		case maxTS > start:
			// The boundary ns may be cut mid-timestamp: carry its entries
			// into the next page's de-dup set and re-request from it.
			seen = map[string]struct{}{}
			for _, e := range entries {
				if e.ns == maxTS {
					seen[e.key()] = struct{}{}
				}
			}
			start = maxTS
		case fresh > 0:
			for _, e := range entries {
				seen[e.key()] = struct{}{}
			}
		default:
			return nil, fmt.Errorf("logql: more entries share timestamp %d than one page returns — raise WithPageLimit", start)
		}
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

// entry is one fetched line with its Loki timestamp.
type entry struct {
	ns      int64
	line    string
	outcome biz.Outcome
}

// key identifies an entry for boundary de-duplication.
func (e entry) key() string { return strconv.FormatInt(e.ns, 10) + "\x00" + e.line }

// parseStreams decodes a query_range streams response into entries stamped
// with each line's Loki timestamp. A line that is not a biz outcome fails
// loudly: the selector targets the exporter's streams, so a foreign line
// means a misconfigured target and silently skipping it could undercount
// money.
func parseStreams(body []byte) ([]entry, error) {
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
		return nil, fmt.Errorf("logql: decode: %w", err)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("logql: query status %q", env.Status)
	}
	var out []entry
	for _, s := range env.Data.Result {
		for _, v := range s.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("logql: entry timestamp %q: %w", v[0], err)
			}
			o, err := eventline.Parse([]byte(v[1]), time.Unix(0, ns).UTC())
			if err != nil {
				return nil, err
			}
			out = append(out, entry{ns: ns, line: v[1], outcome: o})
		}
	}
	return out, nil
}
