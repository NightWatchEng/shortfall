// Package spl is an events-only query.Querier backed by Splunk: it runs an
// export search over the outcome events the splunkhec exporter shipped
// (sourcetype shortfall:outcome), parses each result's raw biz.* JSON, and
// delegates aggregation to the in-memory reference (query/memq), so its
// numbers agree with memq by construction.
//
// Splunk's metric store is a different surface, so Capabilities declares
// Metrics=false: the engine grounds realized and customers here and reads
// the metric legs from a metrics backend.
//
// Nested module, stdlib only: the export endpoint streams NDJSON.
package spl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

// Querier reads outcome events back from a Splunk base URL.
type Querier struct {
	base       string
	token      string
	doer       Doer
	index      string
	sourcetype string
}

var _ query.Querier = (*Querier)(nil)

// Option configures the Querier.
type Option func(*Querier)

// WithHTTPClient injects the HTTP doer (default 60s *http.Client — export
// searches stream).
func WithHTTPClient(d Doer) Option { return func(q *Querier) { q.doer = d } }

// WithIndex sets the index searched (default "main").
func WithIndex(i string) Option { return func(q *Querier) { q.index = i } }

// WithSourcetype overrides the sourcetype searched (default the splunkhec
// exporter's "shortfall:outcome").
func WithSourcetype(s string) Option { return func(q *Querier) { q.sourcetype = s } }

// New builds a Querier for a Splunk base URL (e.g. "https://splunk:8089")
// and an authentication token (Bearer).
func New(baseURL, token string, opts ...Option) *Querier {
	q := &Querier{
		base:       strings.TrimRight(baseURL, "/"),
		token:      token,
		doer:       &http.Client{Timeout: 60 * time.Second},
		index:      "main",
		sourcetype: "shortfall:outcome",
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

// QueryMetric is unsupported: the biz_* families live in a metrics backend.
func (q *Querier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, query.ErrUnsupported
}

// QueryEvents export-searches the window's outcome events and aggregates
// them exactly as memq would.
func (q *Querier) QueryEvents(ctx context.Context, qy query.EventQuery) (query.EventGroups, error) {
	events, err := q.fetch(ctx, qy)
	if err != nil {
		return nil, err
	}
	return memq.New(memq.WithEvents(events)).QueryEvents(ctx, qy)
}

// searchString is the SPL the export job runs: the index and sourcetype
// bound the scan; every EventQuery filter is applied after the parse by the
// reference, so the search stays one reviewed shape.
func (q *Querier) searchString() string {
	return fmt.Sprintf("search index=%q sourcetype=%q | fields _time _raw", q.index, q.sourcetype)
}

// fetch POSTs /services/search/jobs/export and parses the streamed NDJSON
// results: each result's _raw is one biz.* outcome line, stamped from
// Splunk's _time.
func (q *Querier) fetch(ctx context.Context, qy query.EventQuery) ([]biz.Outcome, error) {
	form := url.Values{}
	form.Set("search", q.searchString())
	form.Set("output_mode", "json")
	// Bounds widen by a second on each side: Splunk's latest is exclusive
	// and Unix() floors, so a tight bound could exclude events inside the
	// half-open window; the reference re-applies the exact [From, To).
	form.Set("earliest_time", strconv.FormatInt(qy.Range.From.Unix()-1, 10))
	form.Set("latest_time", strconv.FormatInt(qy.Range.To.Unix()+1, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		q.base+"/services/search/jobs/export", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("spl: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+q.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := q.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spl: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spl: status %d", resp.StatusCode)
	}

	var out []biz.Outcome
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			Preview bool `json:"preview"`
			Result  *struct {
				Raw  string `json:"_raw"`
				Time string `json:"_time"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("spl: decode result row: %w", err)
		}
		if row.Preview || row.Result == nil {
			continue // preview rows and end-of-stream markers carry no data
		}
		at, err := parseSplunkTime(row.Result.Time)
		if err != nil {
			return nil, err
		}
		o, err := eventline.Parse([]byte(row.Result.Raw), at)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("spl: stream: %w", err)
	}
	return out, nil
}

// parseSplunkTime accepts Splunk's _time renderings: RFC3339 with or
// without sub-seconds and numeric-offset forms.
func parseSplunkTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000-07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("spl: unparsable _time %q", s)
}
