// Package sql is an events-only query.Querier over a SQL table of outcome
// events (the ledger the reconciler also writes). It implements QueryEvents
// with parameterized GROUP BY / SUM / COUNT / COUNT(DISTINCT) matching the
// in-memory reference (query/memq), and reports QueryMetric as unsupported —
// a relational store is an event source, not a metric TSDB, so the engine
// reads the metric-derived legs from a TSDB adapter and the customers/realized
// legs from here.
//
// Nested module: the caller brings the *database/sql driver (this package
// imports only database/sql); a non-SQL user pulls no driver.
//
// Expected schema (override the table name with WithTable):
//
//	CREATE TABLE biz_outcomes (
//	  flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
//	  kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER,
//	  at INTEGER  -- event time as Unix nanoseconds
//	);
package sql

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/NightWatchEng/shortfall/query"
)

// labelColumns is the allowlist mapping a query label to its column. Only
// these keys may appear in Filters or GroupBy; anything else is rejected,
// which is also what keeps identifiers out of the SQL string (values are
// always bound as parameters).
var labelColumns = map[string]string{
	"flow":     "flow",
	"stage":    "stage",
	"outcome":  "outcome",
	"currency": "currency",
	"segment":  "segment",
	"kind":     "kind",
	"customer": "customer_id",
	"entity":   "entity_id",
}

// Querier queries a SQL outcomes table.
type Querier struct {
	db             *stdsql.DB
	table          string
	eventHistWeeks int
}

var _ query.Querier = (*Querier)(nil)

// Option configures the Querier.
type Option func(*Querier)

// WithTable overrides the outcomes table name (default "biz_outcomes").
func WithTable(name string) Option { return func(q *Querier) { q.table = name } }

// WithEventHistoryWeeks declares the store's retention (for Caps).
func WithEventHistoryWeeks(w int) Option { return func(q *Querier) { q.eventHistWeeks = w } }

// New builds a Querier over db. The table name is validated as a bare
// identifier at construction so it is never a SQL-injection vector.
func New(db *stdsql.DB, opts ...Option) (*Querier, error) {
	q := &Querier{db: db, table: "biz_outcomes", eventHistWeeks: 8}
	for _, o := range opts {
		o(q)
	}
	if !isBareIdentifier(q.table) {
		return nil, fmt.Errorf("sql: invalid table name %q", q.table)
	}
	return q, nil
}

// Capabilities: events only.
func (q *Querier) Capabilities() query.Caps {
	return query.Caps{Metrics: false, Events: true, EventHistoryWeeks: q.eventHistWeeks}
}

// QueryMetric is unsupported: a relational ledger is an event store, not a
// metric TSDB.
func (q *Querier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, query.ErrUnsupported
}

// QueryEvents runs the grouped event query. Money never crosses a currency:
// when money is read per group (EventAggGroups' sum or EventAggMaxPerGroup's
// max) currency must be grouped or pinned, matching memq and ADR-0001/0009.
func (q *Querier) QueryEvents(ctx context.Context, qy query.EventQuery) (query.EventGroups, error) {
	groupCols, err := columns(qy.GroupBy)
	if err != nil {
		return nil, err
	}
	whereSQL, args, err := whereClause(qy)
	if err != nil {
		return nil, err
	}

	if qy.Agg == query.EventAggDistinctCount {
		return q.distinctCount(ctx, groupCols, whereSQL, args)
	}
	if err := currencyInvariant(qy); err != nil {
		return nil, err
	}
	return q.groups(ctx, qy, groupCols, whereSQL, args)
}

func (q *Querier) distinctCount(ctx context.Context, groupCols []string, whereSQL string, args []any) (query.EventGroups, error) {
	// With no GroupBy the distinct count of empty tuples is 1 if any row
	// matches, else 0 (memq canonicalizes every event's empty key to the same
	// value). SELECT DISTINCT 1 collapses all matching rows to one, so
	// COUNT(*) is 1/0 — NOT the row count. With GroupBy it is the count of
	// distinct column combinations.
	inner := "SELECT DISTINCT 1"
	if len(groupCols) > 0 {
		inner = "SELECT DISTINCT " + strings.Join(groupCols, ", ")
	}
	sqlText := fmt.Sprintf("SELECT COUNT(*) FROM (%s FROM %s%s)", inner, q.table, whereSQL)
	var n int64
	if err := q.db.QueryRowContext(ctx, sqlText, args...).Scan(&n); err != nil {
		return nil, fmt.Errorf("sql: distinct count: %w", err)
	}
	return query.EventGroups{{Count: n}}, nil
}

func (q *Querier) groups(ctx context.Context, qy query.EventQuery, groupCols []string, whereSQL string, args []any) (query.EventGroups, error) {
	wantMax := qy.Agg == query.EventAggMaxPerGroup
	sel := "COUNT(*), COALESCE(SUM(amount_minor), 0)"
	if wantMax {
		sel += ", COALESCE(MAX(amount_minor), 0)" // representative per group (ADR-0009)
	}
	if len(groupCols) > 0 {
		sel = strings.Join(groupCols, ", ") + ", " + sel
	}
	sqlText := fmt.Sprintf("SELECT %s FROM %s%s", sel, q.table, whereSQL)
	if len(groupCols) > 0 {
		sqlText += " GROUP BY " + strings.Join(groupCols, ", ")
	}
	if ord := orderClause(qy.OrderBy, tiebreakCols(qy.GroupBy)); ord != "" {
		sqlText += ord
	} else if qy.Limit > 0 {
		return nil, fmt.Errorf("sql: Limit requires an OrderBy (OrderNone + Limit>0 is undefined)")
	}
	if qy.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", qy.Limit)
	}

	rows, err := q.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sql: group query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out query.EventGroups
	for rows.Next() {
		scanTargets := make([]any, 0, len(groupCols)+2)
		keyVals := make([]stdsql.NullString, len(groupCols))
		for i := range groupCols {
			scanTargets = append(scanTargets, &keyVals[i])
		}
		var count, sum, maxAmt int64
		scanTargets = append(scanTargets, &count, &sum)
		if wantMax {
			scanTargets = append(scanTargets, &maxAmt)
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, fmt.Errorf("sql: scan: %w", err)
		}
		key := map[string]string{}
		for i, col := range qy.GroupBy {
			key[col] = keyVals[i].String
		}
		eg := query.EventGroup{Key: key, Count: count, SumMinor: sum}
		if wantMax {
			eg.MaxMinor = maxAmt
		}
		out = append(out, eg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sql: rows: %w", err)
	}
	return out, nil
}

// columns maps label keys to their columns via the allowlist, preserving order.
func columns(labels []string) ([]string, error) {
	cols := make([]string, 0, len(labels))
	for _, l := range labels {
		c, ok := labelColumns[l]
		if !ok {
			return nil, fmt.Errorf("sql: unknown group/filter label %q", l)
		}
		cols = append(cols, c)
	}
	return cols, nil
}

// whereClause builds the WHERE from Filters (allowlisted columns, bound
// values) plus the half-open [From, To) range on `at` (Unix nanoseconds).
func whereClause(qy query.EventQuery) (string, []any, error) {
	conds := []string{"at >= ?", "at < ?"}
	args := []any{qy.Range.From.UnixNano(), qy.Range.To.UnixNano()}

	// Sort filter keys for a deterministic statement.
	keys := make([]string, 0, len(qy.Filters))
	for k := range qy.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		col, ok := labelColumns[k]
		if !ok {
			return "", nil, fmt.Errorf("sql: unknown filter label %q", k)
		}
		conds = append(conds, col+" = ?")
		args = append(args, qy.Filters[k])
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// orderClause renders the ORDER BY. tiebreakCols is a deterministic secondary
// sort that matches memq's key tiebreak, so a Limit keeps the same groups the
// reference would.
func orderClause(o query.EventOrder, tiebreakCols []string) string {
	var primary string
	switch o {
	case query.OrderSumDesc:
		primary = "SUM(amount_minor) DESC"
	case query.OrderCountDesc:
		primary = "COUNT(*) DESC"
	default:
		return ""
	}
	terms := []string{primary}
	for _, c := range tiebreakCols {
		terms = append(terms, c+" ASC")
	}
	return " ORDER BY " + strings.Join(terms, ", ")
}

// tiebreakCols returns the group columns ordered to match memq's canonical
// key tiebreak, which sorts by label NAME (not GroupBy order). So a Limit on a
// multi-key ordered query keeps the same groups the reference would.
func tiebreakCols(groupBy []string) []string {
	names := append([]string(nil), groupBy...)
	sort.Strings(names)
	cols := make([]string, 0, len(names))
	for _, n := range names {
		if c, ok := labelColumns[n]; ok {
			cols = append(cols, c)
		}
	}
	return cols
}

// currencyInvariant refuses money that could cross currencies (a sum or a max)
// unless currency is grouped or pinned (ADR-0001/0009), matching memq.
func currencyInvariant(qy query.EventQuery) error {
	if _, pinned := qy.Filters["currency"]; pinned {
		return nil
	}
	for _, g := range qy.GroupBy {
		if g == "currency" {
			return nil
		}
	}
	return fmt.Errorf("sql: event sum would cross currencies — pin currency in Filters or add it to GroupBy")
}

// isBareIdentifier allows [A-Za-z_][A-Za-z0-9_]* so a configured table name
// can never carry SQL.
func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
