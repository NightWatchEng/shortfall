// Package promql is a metrics-only query.Querier backed by a Prometheus HTTP
// API endpoint. It translates the frozen query AST into PromQL chosen to match
// the in-memory reference (query/memq) rather than PromQL's rate helpers:
// counter families as a non-extrapolating cumulative difference across the
// window (each boundary read via last_over_time, one-end series filled to 0),
// the gauge family as last_over_time. A stepped (Step>0) query is one such
// instant expression per forward bucket, assembled client-side. See
// translate() and translateStepped() for the exact expressions, the
// exactness properties, and their limits (monotonic-counter assumption,
// sample-boundary alignment) — numeric parity against a live Prometheus is
// verified by the golden harness for both window and stepped shapes.
//
// It is events-incapable (Prometheus has no event store) and returns
// query.ErrUnsupported for QueryEvents, so the engine reports the customers
// leg NotAvailable rather than a silent zero.
//
// Nested module, stdlib only: the Prometheus HTTP API is JSON, so no client
// library is pulled in.
package promql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

// Doer is the slice of *http.Client this adapter needs (a test seam).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Querier queries a Prometheus HTTP API base URL.
type Querier struct {
	base            string
	doer            Doer
	metricHistWeeks int
}

var _ query.Querier = (*Querier)(nil)

// Option configures the Querier.
type Option func(*Querier)

// WithHTTPClient injects the HTTP doer (default 30s *http.Client).
func WithHTTPClient(d Doer) Option { return func(q *Querier) { q.doer = d } }

// WithMetricHistoryWeeks declares the Prometheus retention (for Caps).
func WithMetricHistoryWeeks(w int) Option { return func(q *Querier) { q.metricHistWeeks = w } }

// New builds a Querier for a Prometheus base URL, e.g.
// "http://prometheus:9090".
func New(baseURL string, opts ...Option) *Querier {
	q := &Querier{base: strings.TrimRight(baseURL, "/"), doer: &http.Client{Timeout: 30 * time.Second}, metricHistWeeks: 6}
	for _, o := range opts {
		o(q)
	}
	return q
}

// Capabilities: metrics only.
func (q *Querier) Capabilities() query.Caps {
	return query.Caps{Metrics: true, Events: false, MetricHistoryWeeks: q.metricHistWeeks}
}

// QueryEvents is unsupported: Prometheus is a metric TSDB, not an event store.
func (q *Querier) QueryEvents(context.Context, query.EventQuery) (query.EventGroups, error) {
	return nil, query.ErrUnsupported
}

// QueryMetric translates the query to PromQL and executes it against the
// Prometheus HTTP API, returning the memq Series shape. A Step==0 query is
// one instant query; a stepped query is one instant query per bucket,
// assembled client-side (see translateStepped).
func (q *Querier) QueryMetric(ctx context.Context, qy query.Query) (query.Series, error) {
	if !qy.Range.To.After(qy.Range.From) {
		return nil, fmt.Errorf("promql: empty range [%s,%s)", qy.Range.From, qy.Range.To)
	}
	if qy.Step > 0 {
		return q.stepped(ctx, qy)
	}
	ex, err := translate(qy)
	if err != nil {
		return nil, err
	}
	return q.instant(ctx, ex)
}

// steppedConcurrency bounds the bucket fan-out: enough overlap to amortize
// per-request latency over the engine's multi-week baseline queries without
// stampeding the backend.
const steppedConcurrency = 8

// stepped executes one instant query per bucket — up to steppedConcurrency
// in flight at once — and merges the per-bucket vectors into Series: points
// stamped at each bucket's start (memq's stamp), series ordered by their
// sorted label key (memq's canonical order), identical to a sequential
// execution. Zero-difference counter buckets are dropped — memq omits
// sample-less buckets — while gauge levels, zero included, are kept
// (translateStepped documents the tolerance). On failure the remaining
// buckets are cancelled and the failing bucket's own error surfaces, never
// an induced cancellation.
func (q *Querier) stepped(ctx context.Context, qy query.Query) (query.Series, error) {
	buckets, err := translateStepped(qy)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	vecs := make([]query.Series, len(buckets))
	errs := make([]error, len(buckets))
	sem := make(chan struct{}, steppedConcurrency)
	var wg sync.WaitGroup
	for i, b := range buckets {
		wg.Add(1)
		go func(i int, b steppedBucket) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			vec, err := q.instant(ctx, b.ex)
			if err != nil {
				errs[i] = err
				cancel()
				return
			}
			vecs[i] = vec
		}(i, b)
	}
	wg.Wait()
	// Prefer the first real failure over cancellations it induced.
	var firstErr error
	for _, err := range errs {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
			break
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}

	gauge := gaugeFamilies[qy.Metric]
	merged := map[string]*query.SeriesSlice{}
	var keys []string
	for i, b := range buckets {
		for _, ss := range vecs[i] {
			if len(ss.Points) == 0 || (!gauge && ss.Points[0].Value == 0) {
				continue
			}
			k := sortedLabelKey(ss.Labels)
			s, ok := merged[k]
			if !ok {
				s = &query.SeriesSlice{Labels: ss.Labels}
				merged[k] = s
				keys = append(keys, k)
			}
			s.Points = append(s.Points, query.Point{At: b.start, Value: ss.Points[0].Value})
		}
	}
	sort.Strings(keys)
	out := make(query.Series, 0, len(keys))
	for _, k := range keys {
		out = append(out, *merged[k])
	}
	return out, nil
}

// sortedLabelKey renders a label set as a stable sorted key for merge order.
func sortedLabelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(';')
	}
	return b.String()
}

// promResponse is the Prometheus HTTP API envelope.
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

func (q *Querier) instant(ctx context.Context, ex promExpr) (query.Series, error) {
	v := url.Values{}
	v.Set("query", ex.expr)
	v.Set("time", formatTime(ex.at))
	body, err := q.get(ctx, "/api/v1/query", v)
	if err != nil {
		return nil, err
	}
	return parseVector(body)
}

func (q *Querier) get(ctx context.Context, path string, v url.Values) ([]byte, error) {
	u := q.base + path + "?" + v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("promql: request: %w", err)
	}
	resp, err := q.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("promql: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("promql: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("promql: status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// parseVector parses an instant-query vector into a Series (one point each).
func parseVector(body []byte) (query.Series, error) {
	env, err := decodeEnvelope(body)
	if err != nil {
		return nil, err
	}
	out := make(query.Series, 0, len(env.Data.Result))
	for _, raw := range env.Data.Result {
		var r struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("promql: decode vector sample: %w", err)
		}
		ts, val, err := sample(r.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, query.SeriesSlice{Labels: r.Metric, Points: []query.Point{{At: ts, Value: val}}})
	}
	return out, nil
}

func decodeEnvelope(body []byte) (promResponse, error) {
	var env promResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return promResponse{}, fmt.Errorf("promql: decode envelope: %w", err)
	}
	if env.Status != "success" {
		return promResponse{}, fmt.Errorf("promql: query failed: %s", env.Error)
	}
	return env, nil
}

// sample decodes a Prometheus [timestamp, "value"] pair. The value is a
// float64 (a TSDB stores floats); the engine owns reading money out of it.
func sample(pair [2]json.RawMessage) (time.Time, float64, error) {
	var tsF float64
	if err := json.Unmarshal(pair[0], &tsF); err != nil {
		return time.Time{}, 0, fmt.Errorf("promql: decode timestamp: %w", err)
	}
	var valStr string
	if err := json.Unmarshal(pair[1], &valStr); err != nil {
		return time.Time{}, 0, fmt.Errorf("promql: decode value: %w", err)
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("promql: parse value %q: %w", valStr, err)
	}
	// Reject NaN/±Inf: Prometheus can return these as literal strings, and a
	// non-finite value poisons every downstream sum. memq never produces one.
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return time.Time{}, 0, fmt.Errorf("promql: non-finite value %q", valStr)
	}
	sec := int64(tsF)
	nsec := int64((tsF - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC(), val, nil
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}
