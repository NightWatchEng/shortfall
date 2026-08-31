// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package cwinsights is an events-only query.Querier backed by CloudWatch
// Logs Insights: it queries the log group the cloudwatch exporter's EMF
// records land in, keeps only the records marked event="biz.outcome" (the
// same group carries the exporter's EMF metric records — the marker exists
// to tell them apart), parses each biz.* line, and delegates aggregation to
// the in-memory reference (query/memq), so its numbers agree with memq by
// construction.
//
// The metric legs come from CloudWatch's metric store via a different API
// family, so Capabilities declares Metrics=false: the engine grounds
// realized and customers here.
//
// Nested module, stdlib only: the Logs API is JSON-RPC over HTTPS with
// SigV4 (implemented in sigv4.go); static credentials suffice for
// LocalStack, the conformance backend.
package cwinsights

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Querier reads outcome events back from a CloudWatch Logs Insights
// endpoint.
type Querier struct {
	endpoint     string
	region       string
	logGroup     string
	creds        credentials
	doer         Doer
	pollInterval time.Duration
	now          func() time.Time
}

var _ query.Querier = (*Querier)(nil)

// Option configures the Querier.
type Option func(*Querier)

// WithHTTPClient injects the HTTP doer (default 30s *http.Client).
func WithHTTPClient(d Doer) Option { return func(q *Querier) { q.doer = d } }

// WithEndpoint overrides the API endpoint (LocalStack, VPC endpoints);
// default is the region's https://logs.<region>.amazonaws.com.
func WithEndpoint(u string) Option {
	return func(q *Querier) { q.endpoint = strings.TrimRight(u, "/") }
}

// WithPollInterval sets the GetQueryResults poll cadence (default 250ms).
func WithPollInterval(d time.Duration) Option { return func(q *Querier) { q.pollInterval = d } }

// WithClock injects the signing time source (tests need determinism).
func WithClock(now func() time.Time) Option { return func(q *Querier) { q.now = now } }

// New builds a Querier for a region, the log group the cloudwatch exporter
// writes to, and static credentials.
func New(region, logGroup, accessKey, secretKey string, opts ...Option) *Querier {
	q := &Querier{
		endpoint:     "https://logs." + region + ".amazonaws.com",
		region:       region,
		logGroup:     logGroup,
		creds:        credentials{AccessKey: accessKey, SecretKey: secretKey},
		doer:         &http.Client{Timeout: 30 * time.Second},
		pollInterval: 250 * time.Millisecond,
		now:          time.Now,
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

// QueryMetric is unsupported: the biz_* families live in CloudWatch's
// metric store, a different API family.
func (q *Querier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, query.ErrUnsupported
}

// QueryEvents runs one Insights query over the window and aggregates the
// parsed outcomes exactly as memq would.
func (q *Querier) QueryEvents(ctx context.Context, qy query.EventQuery) (query.EventGroups, error) {
	events, err := q.fetch(ctx, qy)
	if err != nil {
		return nil, err
	}
	return memq.New(memq.WithEvents(events)).QueryEvents(ctx, qy)
}

// queryString is the one reviewed Insights query: the event filter is also
// applied client-side (LocalStack ignores filter clauses), so pushing it
// down is an efficiency for real AWS, never the correctness boundary.
const queryString = `fields @message, @timestamp | filter event = "biz.outcome" | limit 10000`

func (q *Querier) fetch(ctx context.Context, qy query.EventQuery) ([]biz.Outcome, error) {
	start, err := q.call(ctx, "StartQuery", map[string]any{
		"logGroupName": q.logGroup,
		// Insights bounds are inclusive seconds; the reference re-applies
		// the exact half-open [From, To) after the parse.
		"startTime":   qy.Range.From.Unix() - 1,
		"endTime":     qy.Range.To.Unix() + 1,
		"queryString": queryString,
	})
	if err != nil {
		return nil, err
	}
	var started struct {
		QueryID string `json:"queryId"`
	}
	if err := json.Unmarshal(start, &started); err != nil || started.QueryID == "" {
		return nil, fmt.Errorf("cwinsights: start query: no queryId in %s", start)
	}

	for {
		raw, err := q.call(ctx, "GetQueryResults", map[string]any{"queryId": started.QueryID})
		if err != nil {
			return nil, err
		}
		var res struct {
			Status  string             `json:"status"`
			Results [][]insightsColumn `json:"results"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("cwinsights: decode results: %w", err)
		}
		switch res.Status {
		case "Complete":
			// Insights hard-caps results at the query's limit (10000): a
			// full page means the window's events were truncated server-side
			// and any aggregate would silently understate money.
			if len(res.Results) >= 10000 {
				return nil, fmt.Errorf("cwinsights: query returned the 10000-row Insights cap — the window is truncated; narrow the window")
			}
			return parseRows(res.Results)
		case "Scheduled", "Running":
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(q.pollInterval):
			}
		default:
			return nil, fmt.Errorf("cwinsights: query status %q", res.Status)
		}
	}
}

// insightsColumn is one {field, value} pair; LocalStack renders values as
// raw JSON scalars (numbers stay numbers), real AWS as strings.
type insightsColumn struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

// parseRows keeps only rows self-marked as outcome events (the group also
// holds the exporter's EMF metric records); a marked row that fails to
// parse is a loud error, never a silent skip.
func parseRows(rows [][]insightsColumn) ([]biz.Outcome, error) {
	var out []biz.Outcome
	for _, row := range rows {
		var message string
		var at time.Time
		for _, col := range row {
			switch col.Field {
			case "@message":
				if err := json.Unmarshal(col.Value, &message); err != nil {
					return nil, fmt.Errorf("cwinsights: @message: %w", err)
				}
			case "@timestamp":
				t, err := parseTimestamp(col.Value)
				if err != nil {
					return nil, err
				}
				at = t
			}
		}
		// Unmarked rows are skipped by design, a deliberate trade the
		// sibling adapters do not make: this group legitimately carries the
		// exporter's EMF metric records, and a shared group can carry
		// foreign non-JSON lines that only real AWS's pushed-down filter
		// excludes (LocalStack ignores it). The cost accepted here: a
		// mangled outcome record whose marker was destroyed is skipped,
		// not surfaced.
		var marker struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(message), &marker); err != nil || marker.Event != "biz.outcome" {
			continue
		}
		o, err := eventline.Parse([]byte(message), at)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// parseTimestamp accepts both renderings of @timestamp: epoch milliseconds
// (LocalStack) and "2006-01-02 15:04:05.000" UTC (real AWS).
func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	var ms int64
	if err := json.Unmarshal(raw, &ms); err == nil {
		return time.UnixMilli(ms).UTC(), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.UnixMilli(ms).UTC(), nil
		}
		if t, err := time.Parse("2006-01-02 15:04:05.000", s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cwinsights: unparsable @timestamp %s", raw)
}

// call issues one signed JSON-RPC request to the Logs API.
func (q *Querier) call(ctx context.Context, action string, body map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.endpoint+"/", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("cwinsights: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Logs_20140328."+action)
	signV4(req, raw, q.creds, q.region, q.now())
	resp, err := q.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cwinsights: %s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cwinsights: %s read: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cwinsights: %s: status %d: %s", action, resp.StatusCode, out)
	}
	return out, nil
}
