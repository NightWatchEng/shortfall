# Adapters & the capability matrix

shortfall is vendor-neutral: the engine talks to two boundaries — a **read**
boundary (`query.Querier`) for computing reports and a **write** boundary
(`emit.Exporter`) for shipping telemetry. Adapters implement those boundaries
for specific backends. Each adapter honestly declares its **capabilities** so
the engine degrades a leg it cannot ground rather than fabricating a zero.

## Two signal kinds

- **Metrics** — bounded-cardinality counters and gauges (`biz_value_total`,
  `biz_txn_total`, `biz_inflight_value`, `biz_inflight_count`,
  `biz_provider_calls_total`). Cheap, always-on, aggregate.
- **Events** — one record per terminal outcome, carrying the exact amount and
  the entity/customer ids. The exact source of truth; needed for de-dup and
  per-customer breakdowns.

A backend may serve one, the other, or both (`query.Caps{Metrics, Events}`).

## Which leg needs which signal

| Leg | Needs | Notes |
|---|---|---|
| **Realized loss** | Events (preferred) or Metrics | Events give exact per-entity de-dup (ADR-0009); metrics-only is an upper bound, not de-duped |
| **Deferred value** | Metrics | `biz_inflight_value` (+ `biz_inflight_count` for exact txn/breach counts, ADR-0012) |
| **Customer impact** | Events | distinct entities, segments, top accounts — a time series cannot break these out |
| **Unrealized loss** | Metrics | hour-of-week baseline from `biz_txn_total` history (needs `MetricHistoryWeeks` ≥ the flow's lookback) |
| **Coverage** | Metrics or Events, **plus a ledger** | telemetry captured value vs the reconciled ledger |
| **Suggested severity** | (derived) | from the realized + deferred legs, no extra signal |

So an **events-only** backend grounds realized + customers; a **metrics-only**
backend grounds deferred + unrealized + a metrics realized upper bound; a
**both** backend grounds everything.

## Query adapters (read boundary)

| Adapter | Metrics | Events | Notes |
|---|---|---|---|
| `query/memq` | ✅ | ✅ | in-memory reference; the conformance oracle every other adapter is checked against |
| `adapters/query/promql` | ✅ | — | Prometheus; gauges via `last_over_time`, counters via a non-extrapolating `@`-diff |
| `adapters/query/sql` | — | ✅ | any `database/sql` outcomes table; the events source (and the ledger source for coverage) |
| `adapters/query/cwinsights` | — | ✅ | CloudWatch Logs Insights over the cloudwatch exporter's EMF records (stdlib SigV4); metric legs come from CloudWatch's metric store |

Wiring a read boundary — each adapter is one constructor away from
`engine.Compute` (either querier alone grounds its legs; the CLI shows how
to route both at once):

```go
// metrics legs
q := promql.New("http://prom:9090")
report, err := engine.Compute(ctx, &reg, q, req)
if err != nil {
    log.Fatal(err)
}
fmt.Println(report.Severity)
```

```go
// event legs — the adapter's package is named sql, so alias it:
//   sqlq "github.com/NightWatchEng/shortfall/adapters/query/sql"
db, _ := sql.Open("sqlite", "file:outcomes.db")
q, _ := sqlq.New(db)
report, err := engine.Compute(ctx, &reg, q, req)
if err != nil {
    log.Fatal(err)
}
fmt.Println(report.Customers.Distinct)
```

The SQL adapter's expected table (override the name with `WithTable`):

```sql
CREATE TABLE biz_outcomes (
  flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
  kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER, at INTEGER
);
-- `at` is Unix nanoseconds; amount_minor is minor units.
```

## Export adapters (write boundary)

| Adapter | Metrics | Events | Backend |
|---|---|---|---|
| `adapters/export/prometheus` | ✅ | — | Prometheus (scrape) |
| `adapters/export/cloudwatch` | ✅ | ✅ | CloudWatch (EMF / PutMetricData) |

Pair a metrics exporter with an events exporter (or use one that does both) to
ground every leg. The exporter you write ships the same fixed `biz_*` families
— an exporter that does not recognise a family fails loudly rather than
silently dropping the batch (a conformance test pins this).

Wiring a write boundary — hand the exporter to `emit.New` and record stage
transitions as usual:

```go
exp, _ := promexport.New() // owns a private registry by default
em, _ := emit.New(&reg, exp)
http.Handle("/metrics", promhttp.HandlerFor(exp.Gatherer(), promhttp.HandlerOpts{}))
em.Record(ctx, "auth", biz.ResultSuccess)
```

```go
exp := cloudwatch.New()    // EMF metrics + events to CloudWatch Logs
em, _ := emit.New(&reg, exp)
defer em.Close(ctx)
```

## Payment & incident adapters

- `adapters/payment/stripe` — builds the provider-side ledger coverage
  reconciles against: `stripe.Reconcile(ctx, fetch, since)` pages payment
  intents into `biz.LedgerRow`s (capture amount basis, ADR-0010); feed the
  rows to `shortfall reconcile --ledger rows.json`.
- `adapters/incident/slack` — posts and refreshes the impact ledger in the
  incident channel: `slack.New(token).Post(ctx, channel, report)`, or
  `Refresh` to keep one message live while the incident evolves.
- `adapters/incident/{incidentio,rootly,firehydrant,pagerduty}` — thin
  impact-field writers (consumers of the number, not producers): each
  `WriteImpact(ctx, incidentID, report)` writes the vendor's impact/custom
  field with the one-line `report.Summary` (realized, deferred, unrealized
  range, suggested severity — evidence-tagged, never merged). Targets:
  incident.io a text custom field, PagerDuty a custom field by name,
  FireHydrant its native `customer_impact_summary` (or a custom field),
  Rootly the incident `summary` attribute. FireHydrant and PagerDuty also
  `AttachCustomersCSV` — the vendor-neutral `report.CustomersCSV` top-accounts
  export — as an incident note. All are SDK-free `*http.Client` writers with
  overridable base URLs, httptest-verified.

## Writing your own adapter

Implement `query.Querier` (three methods) or `emit.Exporter`, declare honest
`Capabilities()`, and run the shared conformance suite
(`testkit/conformance`) — it checks your adapter against the `memq` reference so
"same numbers on a real backend" is a test, not a hope.
