# Backends & adapters

The engine talks to two boundaries and nothing else: a **read** boundary
(`query.Querier`) for computing reports, and a **write** boundary
(`emit.Exporter`) for shipping telemetry. Adapters implement those for
specific backends, and each declares its **capabilities** honestly, so
the engine degrades a leg it cannot ground rather than fabricating a
zero.

Each adapter is its own nested Go module. You compile the ones you use.

## Two signal kinds

- **Metrics** — bounded-cardinality counters and gauges
  (`biz_value_total`, `biz_txn_total`, `biz_inflight_value`,
  `biz_inflight_count`, `biz_provider_calls_total`). Cheap, always-on,
  aggregate.
- **Events** — one record per terminal outcome, carrying the exact
  amount and the entity and customer ids. Needed for de-duplication and
  per-customer breakdowns.

A backend may serve one, the other, or both (`query.Caps{Metrics, Events}`).

## Which leg needs which signal

| Leg | Needs | Notes |
|---|---|---|
| **Realized loss** | Events (preferred) or Metrics | Events give exact per-entity de-dup (ADR-0009); metrics-only is an upper bound, not de-duped |
| **Deferred value** | Metrics | `biz_inflight_value`, plus `biz_inflight_count` for exact txn and breach counts (ADR-0012) |
| **Customer impact** | Events | distinct entities, segments, top accounts — a time series cannot break these out |
| **Unrealized loss** | Metrics | hour-of-week baseline from `biz_txn_total` history (needs `MetricHistoryWeeks` ≥ the flow's lookback) |
| **Coverage** | Metrics or Events, **plus a ledger** | telemetry captured value vs the reconciled ledger |
| **Suggested severity** | (derived) | from realized + deferred; no extra signal |

So an **events-only** backend grounds realized and customers; a
**metrics-only** backend grounds deferred, unrealized and a realized
upper bound; **both** grounds everything.

## Query adapters — the read boundary

| Adapter | Metrics | Events | Notes |
|---|---|---|---|
| `query/memq` | ✅ | ✅ | in-memory reference; the conformance oracle every other adapter is checked against |
| `adapters/query/promql` | ✅ | — | Prometheus; gauges via `last_over_time`, counters via a non-extrapolating `@`-diff |
| `adapters/query/sql` | — | ✅ | any `database/sql` outcomes table; also the ledger source for coverage |
| `adapters/query/cwinsights` | — | ✅ | CloudWatch Logs Insights over the cloudwatch exporter's EMF records (stdlib SigV4) |
| `adapters/query/gcplogging` | — | ✅ | Cloud Logging entries, read as SQL through the Log Analytics bucket's linked BigQuery dataset (stdlib REST) |

Each adapter is one constructor away from `engine.Compute`:

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

```go
// GCP event legs. Credentials ride the injected HTTP client or
// WithBearerToken; there is no cloud SDK in the module.
gq, err := gcplogging.New("my-project", "logs_analytics", gcplogging.WithLocation("us"))
if err != nil {
    log.Fatal(err)
}
report, err := engine.Compute(ctx, &reg, gq, req)
if err != nil {
    log.Fatal(err)
}
fmt.Println(report.Realized.ByCurrency)
```

**Pairing two read adapters.** No single AWS or GCP read boundary serves
both signals, so the engine gets one adapter per signal kind and each
declares the other unsupported: `cwinsights` or `gcplogging` grounds the
realized and customer legs from events, and `promql` grounds the
deferred, unrealized and baseline legs against a Prometheus-compatible
metrics store (on GCP, Managed Service for Prometheus, fed by
`adapters/export/otlp`). That is why `query.Caps` carries `Metrics` and
`Events` independently: an events-only querier returns
`query.ErrUnsupported` from `QueryMetric`, and the engine turns that into
a leg marked unavailable with a reason. The CLI does the same pairing
behind `--prometheus` and `--sql`.

De-duplication is not the adapter's job. The per-entity de-dup and the
later-success exclusion (ADR-0009) live in the engine; an adapter hands
back the per-entity groups and the success set faithfully, which its
conformance test asserts against `memq` rather than assumes.

The SQL adapter's expected table (override the name with `WithTable`):

```sql
CREATE TABLE biz_outcomes (
  flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
  kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER, at INTEGER
);
-- `at` is Unix nanoseconds; amount_minor is minor units.
```

## Export adapters — the write boundary

| Adapter | Metrics | Events | Backend |
|---|---|---|---|
| `adapters/export/otlp` | ✅ | ✅ | anything an OpenTelemetry Collector fans out to — **the vendor-neutral path** |
| `adapters/export/prometheus` | ✅ | — | Prometheus (scrape) |
| `adapters/export/cloudwatch` | ✅ | ✅ | CloudWatch (EMF / PutMetricData) |
| `adapters/export/gcp` | — | ✅ | Google Cloud (Cloud Logging) |

**OTLP is the answer for any backend without its own adapter here**, and
the one to reach for when you already run a collector: one integration
reaches Datadog, Honeycomb, Grafana, Google Cloud and everything else
the collector fans out to. That is why the supported surface stays small
while the reachable backend set does not.

**On Google Cloud, metrics ship over OTLP.** `adapters/export/gcp`
covers events only and reports `Metrics: false` unconditionally — Cloud
Logging does not extract metrics from log entries the way CloudWatch EMF
does. Its events path needs no credentials at all (structured JSON on
stdout, which the logging agent parses into a `jsonPayload`); pair it
with `adapters/export/otlp` and a collector running the Google Cloud
exporter for the `biz_*` families. The cost of that pairing is the otel
module graph and nothing more.

Metric mapping is fixed by `emit`'s semantics rather than chosen: the
counter families become **delta** monotonic `Sum[int64]`s, because
`emit.MetricPoint` carries a delta and not a running total, and the two
in-flight families become `Gauge[int64]` levels (ADR-0012). Every
instrument is `int64` — a `float64` aggregation would round money
silently above 2^53 minor units, and a test pins that none is ever used.
Each point keeps its own observation time, so a batch delayed by an
incident is not restamped to flush time.

Every exporter that ships metrics rejects a family it does not
recognise rather than guessing a kind for it: shipping an unrecognised
*level* family as a monotonic counter would have the backend sum it,
which is silently wrong arithmetic on money. The three metric exporters
pin that in their own unit tests; the shared `testkit/conformance` suite
covers no-loss, capability honesty and empty batches.

Hand an exporter to `emit.New` and record stage transitions as usual:

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

```go
// GCP: outcome events to Cloud Logging via stdout. The project id is
// optional and buys one thing: a logging.googleapis.com/trace link that
// Cloud Logging correlates with Cloud Trace.
exp := gcp.New(gcp.WithProject("my-project"))
em, _ := emit.New(&reg, exp)
defer em.Close(ctx)
```

```go
// OTLP: both signals to a collector. With no options the standard
// OTEL_EXPORTER_OTLP_* environment is read.
otlpExp, err := otlp.New(ctx)
if err != nil {
    log.Fatal(err)
}
em, _ := emit.New(&reg, otlpExp)
defer em.Close(ctx)
```

## Payment & incident adapters

- `adapters/payment/stripe` — builds the provider-side ledger coverage
  reconciles against. `stripe.Reconcile(ctx, fetch, since)` pages payment
  intents into `biz.LedgerRow`s (capture amount basis, ADR-0010); feed
  the rows to `shortfall reconcile --ledger rows.json`. It also wraps the
  Stripe backend to observe provider calls and maps webhooks to
  `biz.Outcome`s — see the [money path](architecture/money-path.md).
- `adapters/incident/slack` — posts and refreshes the impact ledger in
  the incident channel: `slack.New(token).Post(ctx, channel, report)`,
  or `Refresh` to keep one message live as the incident evolves.
- `adapters/incident/{incidentio,rootly,firehydrant,pagerduty}` — impact
  field writers. Each `WriteImpact(ctx, incidentID, report)` writes the
  vendor's impact or custom field with the one-line `report.Summary`.
  FireHydrant and PagerDuty also `AttachCustomersCSV` — the
  vendor-neutral `report.CustomersCSV` top-accounts export — as an
  incident note. All are SDK-free `*http.Client` writers with
  overridable base URLs. They consume the number; none produces one.

## Writing your own adapter

Implement `query.Querier` (three methods) or `emit.Exporter`, declare
honest `Capabilities()`, and run the shared conformance suite
(`testkit/conformance`) — it checks your adapter against the `memq`
reference, so "same numbers on a real backend" is a test rather than a
hope. The wire-level contract an adapter must satisfy is in
[portability](portability.md).
