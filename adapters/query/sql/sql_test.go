package sql

import (
	"context"
	stdsql "database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
)

var (
	from = time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	to   = time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
)

func ev(minOff int, flow, stage string, result biz.Result, amount int64, currency, customer, segment string) biz.Outcome {
	return biz.Outcome{
		At: from.Add(time.Duration(minOff) * time.Minute), Stage: stage, Result: result,
		VC: biz.ValueContext{Flow: flow, EntityID: customer + "-e", CustomerID: customer, Segment: segment,
			Money: biz.Money{Amount: amount, Currency: currency, Exponent: 2}, Kind: biz.KindFee},
	}
}

// seed creates the schema and inserts the events, returning a Querier + the
// same events for building a parallel memq.
func seed(t *testing.T, events []biz.Outcome) *Querier {
	t.Helper()
	db, err := stdsql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE biz_outcomes (
		flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
		kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER, at INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range events {
		_, err := db.Exec(`INSERT INTO biz_outcomes VALUES (?,?,?,?,?,?,?,?,?,?)`,
			o.VC.Flow, o.Stage, string(o.Result), o.VC.Money.Currency, o.VC.Segment,
			string(o.VC.Kind), o.VC.CustomerID, o.VC.EntityID, o.VC.Money.Amount, o.At.UnixNano())
		if err != nil {
			t.Fatal(err)
		}
	}
	q, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestCapabilitiesAndMetricsUnsupported(t *testing.T) {
	q := seed(t, nil)
	if c := q.Capabilities(); c.Metrics || !c.Events {
		t.Fatalf("caps = %+v, want events-only", c)
	}
	if _, err := q.QueryMetric(context.Background(), query.Query{}); !errors.Is(err, query.ErrUnsupported) {
		t.Fatalf("QueryMetric err = %v, want ErrUnsupported", err)
	}
}

func TestQueryEventsCurrencyInvariant(t *testing.T) {
	q := seed(t, []biz.Outcome{
		ev(1, "invoice.pay", "capture", biz.ResultFailed, 100, "USD", "h:c1", "smb"),
		ev(2, "invoice.pay", "capture", biz.ResultFailed, 50, "EUR", "h:c2", "smb"),
	})
	// Sum without currency grouped or pinned must be refused.
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{Range: rng(), GroupBy: []string{"customer"}}); err == nil {
		t.Fatal("cross-currency sum must be refused")
	}
	// Grouping by currency is fine.
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{Range: rng(), GroupBy: []string{"currency"}}); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownLabelRejected(t *testing.T) {
	q := seed(t, nil)
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{Range: rng(), GroupBy: []string{"drop table"}}); err == nil {
		t.Fatal("an unknown/dangerous label must be rejected (allowlist)")
	}
}

func TestBadTableRejected(t *testing.T) {
	db, _ := stdsql.Open("sqlite", ":memory:")
	t.Cleanup(func() { _ = db.Close() })
	if _, err := New(db, WithTable("outcomes; DROP TABLE x")); err == nil {
		t.Fatal("a non-identifier table name must be rejected")
	}
}

func TestLimitRequiresOrder(t *testing.T) {
	q := seed(t, []biz.Outcome{ev(1, "invoice.pay", "capture", biz.ResultFailed, 100, "USD", "h:c1", "smb")})
	if _, err := q.QueryEvents(context.Background(), query.EventQuery{
		Range: rng(), Filters: map[string]string{"currency": "USD"}, GroupBy: []string{"customer"}, Limit: 1,
	}); err == nil {
		t.Fatal("Limit without OrderBy must error")
	}
}

// TestParityWithMemq is the correctness bar: the same events queried through
// SQLite and through the in-memory reference return identical results — run in
// ordinary CI, no external process.
func TestParityWithMemq(t *testing.T) {
	events := []biz.Outcome{
		ev(1, "invoice.pay", "capture", biz.ResultFailed, 14900, "USD", "h:c1", "smb"),
		ev(2, "invoice.pay", "capture", biz.ResultFailed, 100, "USD", "h:c1", "smb"),
		ev(3, "invoice.pay", "capture", biz.ResultFailed, 900, "USD", "h:c2", "enterprise"),
		ev(4, "invoice.pay", "capture", biz.ResultSuccess, 500, "USD", "h:c3", "smb"),
		ev(5, "invoice.pay", "capture", biz.ResultFailed, 5000, "EUR", "h:c2", "enterprise"),
	}
	sq := seed(t, events)
	mq := memq.New(memq.WithEvents(events))
	ctx := context.Background()

	queries := []query.EventQuery{
		{Range: rng(), Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"currency"}, OrderBy: query.OrderSumDesc},
		{Range: rng(), Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer"}, OrderBy: query.OrderSumDesc, Limit: 2},
		{Range: rng(), Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer", "segment"}},
		{Range: rng(), Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"customer"}, Agg: query.EventAggDistinctCount},
		// Empty-GroupBy distinct count: must be 1 (any match), not the row count.
		{Range: rng(), Filters: map[string]string{"outcome": "failed", "currency": "USD"}, Agg: query.EventAggDistinctCount},
		// OrderCountDesc with a limit.
		{Range: rng(), Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"customer"}, OrderBy: query.OrderCountDesc, Limit: 2},
		// Ordered multi-key (segment,customer): tiebreak must match memq's name-sorted canonical order.
		{Range: rng(), Filters: map[string]string{"outcome": "failed", "currency": "USD"}, GroupBy: []string{"segment", "customer"}, OrderBy: query.OrderSumDesc},
		// Group/filter by kind (parity for the kind label).
		{Range: rng(), Filters: map[string]string{"outcome": "failed"}, GroupBy: []string{"currency", "kind"}},
	}
	for i, qy := range queries {
		sgroups, err := sq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d sql: %v", i, err)
		}
		mgroups, err := mq.QueryEvents(ctx, qy)
		if err != nil {
			t.Fatalf("query %d memq: %v", i, err)
		}
		if !sameGroups(sgroups, mgroups, qy.OrderBy != query.OrderNone) {
			t.Fatalf("query %d parity mismatch:\n sql=%+v\nmemq=%+v", i, sgroups, mgroups)
		}
	}
}

// sameGroups compares two EventGroups; when ordered is true the sequence must
// match, otherwise they are compared as a set keyed by their group key.
func sameGroups(a, b query.EventGroups, ordered bool) bool {
	if len(a) != len(b) {
		return false
	}
	if ordered {
		for i := range a {
			if !eqGroup(a[i], b[i]) {
				return false
			}
		}
		return true
	}
	index := map[string]query.EventGroup{}
	for _, g := range b {
		index[keyStr(g.Key)] = g
	}
	for _, g := range a {
		if !eqGroup(g, index[keyStr(g.Key)]) {
			return false
		}
	}
	return true
}

func eqGroup(a, b query.EventGroup) bool {
	if a.Count != b.Count || a.SumMinor != b.SumMinor || len(a.Key) != len(b.Key) {
		return false
	}
	for k, v := range a.Key {
		if b.Key[k] != v {
			return false
		}
	}
	return true
}

func keyStr(k map[string]string) string {
	s := ""
	for _, name := range []string{"currency", "customer", "segment", "flow", "stage", "outcome", "entity"} {
		if v, ok := k[name]; ok {
			s += name + "=" + v + ";"
		}
	}
	return s
}

func rng() query.TimeRange { return query.TimeRange{From: from, To: to} }
