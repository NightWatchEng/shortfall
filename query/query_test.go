package query

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeQuerier proves the frozen Querier surface is implementable,
// including the honest metrics-only path.
type fakeQuerier struct {
	events bool
}

func (f fakeQuerier) QueryMetric(_ context.Context, q Query) (Series, error) {
	return Series{{Labels: q.Filters, Points: []Point{{At: q.Range.From, Value: 1}}}}, nil
}
func (f fakeQuerier) QueryEvents(context.Context, EventQuery) (EventGroups, error) {
	if !f.events {
		return nil, ErrUnsupported
	}
	return EventGroups{{Count: 1, SumMinor: 14900}}, nil
}
func (f fakeQuerier) Capabilities() Caps { return Caps{Events: f.events, HistoryWeeks: 8} }

var _ Querier = fakeQuerier{}

func TestFrozenQuerierSurface(t *testing.T) {
	window := TimeRange{From: time.Unix(1000, 0), To: time.Unix(2000, 0)}
	cases := []struct {
		name       string
		q          fakeQuerier
		wantErr    error
		wantGroups int
	}{
		{"events-capable backend", fakeQuerier{events: true}, nil, 1},
		{"metrics-only backend says so honestly", fakeQuerier{events: false}, ErrUnsupported, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			series, err := c.q.QueryMetric(context.Background(), Query{Metric: "biz_txn_total", Agg: AggSum, Range: window})
			if err != nil || len(series) != 1 {
				t.Fatalf("metric path: %v %v", series, err)
			}
			groups, err := c.q.QueryEvents(context.Background(), EventQuery{Range: window})
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("events err = %v, want %v", err, c.wantErr)
			}
			if len(groups) != c.wantGroups {
				t.Fatalf("groups = %d, want %d", len(groups), c.wantGroups)
			}
			if got := c.q.Capabilities().Events; got != c.q.events {
				t.Fatalf("capability honesty broken: %v", got)
			}
		})
	}
}
