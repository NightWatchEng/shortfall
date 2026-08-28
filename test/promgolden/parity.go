package promgolden

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/query"
)

// waitIngested polls Prometheus until the given query returns a non-empty
// result (remote-write commits asynchronously), failing the test on timeout.
func waitIngested(t *testing.T, ctx context.Context, q query.Querier, probe query.Query) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, err := q.QueryMetric(ctx, probe)
		if err == nil && len(s) > 0 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("prometheus did not ingest the remote-written samples within 30s")
}

// sameSeries reports whether two results carry the same value per labelled
// series. It compares the summed value per series keyed by labels — not the
// point timestamps: memq stamps a Step==0 bucket at the window start while
// Prometheus stamps the @-evaluation time, and the parity that matters is the
// number, not the instant. Money is integer minor units, so the counter sums
// are exact and compared exactly; the gauge level is likewise integer-valued.
func sameSeries(a, b query.Series) bool {
	return mapsEqual(seriesMap(a), seriesMap(b))
}

func seriesMap(s query.Series) map[string]float64 {
	m := map[string]float64{}
	for _, ss := range s {
		var v float64
		for _, p := range ss.Points {
			v += p.Value
		}
		m[labelKey(ss.Labels)] = v
	}
	return m
}

func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for _, k := range keys {
		s += k + "=" + labels[k] + ","
	}
	return s
}

func mapsEqual(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
