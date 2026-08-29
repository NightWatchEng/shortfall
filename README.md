# shortfall

**What an incident cost, who it hit, and how sure you are.**

A vendor-neutral Go library for incident dollar impact. Instrument once —
attach context where a request enters, call `Record()` per stage — and
for any incident window shortfall answers with four numbers, each
labelled by the kind of evidence behind it:

| Leg | What it is | Evidence |
|---|---|---|
| **Realized loss** | Transactions that failed, summed, de-duplicated by entity | deterministic |
| **Deferred value** | In-flight or backlogged value, by age, with SLA conversion to lost | deterministic |
| **Unrealized loss** | Value that never happened, from a seasonal baseline — always a range | estimate |
| **Customer impact** | Distinct entities, segments, top accounts | deterministic |

plus a **coverage ratio** — telemetry reconciled against your ledger —
because a number Finance cannot audit is a number Finance will not use.

## Why it's easy to adopt

- **Your backend, not a new one.** Signals ship through export adapters
  (Prometheus, CloudWatch EMF) and reports read back through query
  adapters — no new datastore, no agent, no service to run.
- **No dependency bloat.** The core module has no heavy deps; every
  adapter is its own nested Go module, so a Prometheus user never pulls
  a payments SDK.
- **Cheap on the hot path.** One `Record()` is ~650 ns / 3 allocs
  (benchmarks below) and money accounting never depends on trace
  sampling.
- **Numbers you can defend.** Money is `int64` minor units (never
  float), realized and estimated value are never merged, PII is guarded
  by code, and coverage tells you how much telemetry actually saw.

## Get started

Three questions, in order: how value signals get **emitted** from your
services, how they are **gathered** back into an incident answer, and
what ships in the box for both. (The [Quickstart](docs/quickstart.md)
walks the same path hands-on — nothing to a rendered report in 10
minutes with zero external services.)

**How it works, in one breath.** `biz.WithValueContext` attaches the
business facts — flow, entity id, hashed customer, minor-unit amount —
to the request context, and they ride W3C Baggage across service hops.
Every `Record()` turns a stage transition into two signal kinds: bounded
`biz_*` metric families (transaction counts, value) and one unsampled
outcome event carrying the exact amount and ids; queue consumers add the
in-flight backlog gauge through `emit.InFlightTracker`. Your
exporter ships both to the backend you already run — **Prometheus** for
metrics, **CloudWatch** for metrics and events — and at incident time
the engine queries them back and computes the four legs, each labelled
by its evidence.

### 1. Emit — instrument where value flows

Attach business context where a request enters, then record every stage
transition. Everything below is real, resolvable API:

```go
func main() {
    reg, err := registry.Load("registry.yaml") // the Finance-co-signed flow registry
    if err != nil {
        log.Fatal(err)
    }
    em, err := emit.New(&reg, cloudwatch.New()) // EMF exporter — swap for your backend
    if err != nil {
        log.Fatal(err)
    }

    http.HandleFunc("POST /invoices/{invoice}/pay", func(w http.ResponseWriter, r *http.Request) {
        ctx, err := biz.WithValueContext(r.Context(), biz.ValueContext{
            Flow:       "invoice.pay",
            EntityID:   r.PathValue("invoice"), // idempotency key — failures de-dup by entity across retries
            CustomerID: r.Header.Get("X-Account-Hash"), // pre-hashed — raw ids never enter biz.*
            Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
            Kind:       biz.KindFee,
        })
        if err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            return
        }
        em.Record(ctx, "auth", biz.ResultSuccess) // once per stage transition —
        w.WriteHeader(http.StatusAccepted)        // failed/deferred on those paths
    })
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

One `Record()` is ~650 ns / 3 allocs, and money never depends on trace
sampling.

### Your services call other HTTP APIs — what integrates, what leaks?

Payment-path services call HTTP APIs constantly: your own downstream
services, and providers like Stripe. Route both through one client whose
Transport is the egress fence:

```go
// One client per service. biz context (amounts, customer hashes) is
// injected ONLY toward hosts the registry's propagation.allow_hosts
// names; toward everything else — your payment provider included — it
// is stripped, never forwarded.
func newClient(reg *registry.Registry) *http.Client {
    return &http.Client{Transport: httpmw.NewTransport(reg, http.DefaultTransport)}
}
```

- **Your own services** (allowlisted, e.g. `*.internal.example.com`):
  the value context rides along, so the downstream service wraps its
  handlers in `httpmw.Middleware(&reg)` and its `Record()` calls land on
  the same entity and dollars — one flow, measured across every hop.
- **External providers** (not allowlisted): the request leaves clean.
  For provider health, `adapters/payment/stripe`'s wrapped client
  observes each call for the `biz_provider_calls_total` family.
- **Queues instead of HTTP**: the `kafka`, `sqs`, `amqp` carriers do the
  same job on message headers, and consumers wrap their backlog in
  `emit.InFlightTracker`, which publishes the `biz_inflight_value` gauge
  the deferred leg reads.

A worked two-service example (webhook Lambdas → payments-service) lives
in [docs/integration-webhook-lambdas.md](docs/integration-webhook-lambdas.md).

### 2. Gather — ask for the damage

Where step 1 exported decides how you read back:

- **Prometheus (metrics) and/or a SQL outcomes table (events)** — the
  CLI reads both directly. For any incident window:

  ```sh
  shortfall impact --registry registry.yaml \
    --from 2026-08-28T14:00:00Z --to 2026-08-28T15:30:00Z \
    --flow invoice.pay --format markdown \
    --prometheus http://prometheus:9090 \
    --sql "file:outcomes.db"
  ```

  For ad-hoc triage, the same `biz_*` families answer PromQL directly
  (label sets below are verbatim from the exporter's exposition):

  ```promql
  # failure rate per flow, last 5 minutes
  sum by (flow) (rate(biz_txn_total{outcome="failed"}[5m]))
    / sum by (flow) (rate(biz_txn_total[5m]))

  # failed value, minor units per minute
  sum by (flow, currency) (rate(biz_value_total{outcome="failed"}[5m])) * 60

  # deferred backlog by age bucket, right now
  sum by (age_bucket, currency) (biz_inflight_value{flow="invoice.pay"})
  ```

- **CloudWatch** — the EMF records from step 1 are already in CloudWatch
  Logs; a small reporting job reads them back with the `cwinsights`
  querier, hands it to `engine.Compute`, and renders the same report.
  CloudWatch Logs is an event store, so it grounds realized loss and
  customer impact; pair it with a promql-readable metrics store to
  ground the deferred and unrealized legs too.

  For ad-hoc triage, query the outcome records in Logs Insights (the
  `cwinsights` querier automates the same `filter event = "biz.outcome"`
  scan; field names are verbatim from the EMF record):

  ```
  filter event = "biz.outcome" and `biz.outcome` = "failed"
  | stats sum(`biz.amount_minor`) as failed_minor, count(*) as txns
      by `biz.flow`, `biz.currency`
  | sort failed_minor desc
  ```

  ```
  filter event = "biz.outcome" and `biz.outcome` = "failed"
  | stats sum(`biz.amount_minor`) as failed_minor
      by `biz.customer.id`, `biz.segment`
  | sort failed_minor desc | limit 10
  ```

One honesty note covering both query sets: the ad-hoc sums — PromQL
value rates and Logs Insights sums alike — are triage numbers. They are
not de-duplicated by entity, and entities that later succeeded are not
excluded. `shortfall impact` applies both on the events path; that is
the number Finance sees.

Out come the four legs, each labelled by its evidence: **realized loss**
(failed transactions summed, de-duplicated by entity), **deferred
value** (backlog by age, SLA-converted to projected loss), **unrealized
loss** (a range against the seasonal baseline — always an estimate,
never merged with the deterministic legs), and **customer impact**
(distinct entities, segments, top accounts) — plus a suggested severity
from the registry's $/min ladder. Metrics ground the deferred and
unrealized legs; events ground realized de-dup and customers — wire
both signal kinds and every leg is grounded. `shortfall reconcile
--ledger` adds the coverage ratio: telemetry checked against your
provider's ledger. See [adapters.md](docs/adapters.md) for the full
backend-to-leg matrix.

### 3. What ships in the box

| Surface | What you get | Where |
|---|---|---|
| Instrument | `ValueContext`, int64 minor-unit `Money`, the PII guard | `biz` |
| Record | `Record` per stage, `InFlightTracker`/`SetInFlight` backlog gauges, `Flush` | `emit` |
| Propagate | HTTP Baggage middleware + egress-fenced Transport; Kafka, SQS, AMQP carriers | `propagate/*` |
| Export | Prometheus (metrics), CloudWatch EMF (metrics + events) | `adapters/export/*` |
| Query back | `promql` (metrics); `cwinsights`, `sql` (events) | `adapters/query/*` |
| Compute & render | four legs + coverage + suggested severity, as text/JSON/markdown | `engine`, `cmd/shortfall` |
| Reconcile | Stripe ledger reconciler feeding the coverage ratio | `adapters/payment/stripe` |
| Notify | impact writers for PagerDuty, incident.io, FireHydrant, Rootly, Slack | `adapters/incident/*` |

Runnable examples live in the `biz`, `emit`, and `engine` packages
(`go doc`, and pkg.go.dev once the repo is public).

## Benchmarks

Performance is part of the contract — shortfall runs inside your request
path. Apple M-class laptop, Go defaults; CI tracks every PR with
benchstat:

| Path | ns/op | allocs/op |
|---|---:|---:|
| `emit.Record` (accepted) | 647 | 3 |
| `biz.vc` encode / decode | 128 / 187 | 3 / 1 |
| In-flight age bucketing | 0.23 | 0 |
| `engine.Compute`, 200k events | 0.75 s | — |

## Documentation

- [Quickstart](docs/quickstart.md) — `go get` to a rendered impact report in 10 minutes.
- [Adapters & capability matrix](docs/adapters.md) — which backend grounds which leg, with wiring snippets.
- [Example integration: webhook Lambdas → payments-service](docs/integration-webhook-lambdas.md) — a two-system flow end to end, and which leg covers which outage direction.
- [Architecture](docs/architecture/README.md) — C4 diagrams, the money-path sequences, and the repository layout.
- [Registry reference](docs/registry.md) — every field of the flow registry.
- [What is a "dollar" here](docs/money.md) — kind semantics, lost vs delayed, why ranges (for Finance).
- [Semantic conventions (draft)](docs/semconv.md) — the `biz.*` attribute and metric shapes.
- [Design decisions](docs/adr/README.md) — one ADR per irreversible choice.

## Status

Pre-release, under active construction. Nothing here is stable yet.

**Versioning policy:** v0.x — the public interfaces (`biz`, `registry`,
`emit`, `engine`, `query`, `propagate/*`) may change between minor versions.
v1.0.0 will be tagged only after those interfaces have survived two external
adapters not written by the authors.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
