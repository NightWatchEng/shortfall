# shortfall

**What an incident cost, who it hit, and how sure you are.**

shortfall is a Go library that measures the dollar impact of an incident
from telemetry your services already emit, and reports it in a form
Finance can audit.

## The empty field in the postmortem

The incident is over. The timeline is written, the root cause is
understood, and one field in the template is still blank: **customer and
revenue impact**.

It gets filled in the usual way. Someone takes a failure count off a
dashboard, multiplies by an average order value that somebody remembers,
puts a `~` in front of it, and moves on. Finance reads the number, finds
nothing to reconcile it against, and files it as an anecdote. Two
quarters later, when the reliability roadmap needs defending, there is
still no figure anyone trusts enough to cite.

That estimate is not wrong because the person writing it was careless.
It is wrong because a failure count is not money, and four specific
things sit between the two.

**A retry is not a second loss.** Your checkout service retried that
failed capture four times before giving up. The dashboard counted four
failures. The customer lost one payment. Any honest figure has to
collapse those four events onto the entity they share — the payment
intent, the invoice, the order — and a count cannot do that, because by
the time a request becomes a metric increment the identity is gone.

**Delayed money is not lost money.** During the incident your queues
backed up. That value is real and it is at risk, but most of it will
drain in twenty minutes and some of it will breach an SLA and become a
credit you owe. Adding the backlog to your loss figure overstates it.
Ignoring it understates it. The difference between the two is a deadline
that lives in a config file, not in the telemetry.

**The largest loss leaves no record at all.** Checkouts abandoned while
the payment page was timing out never became a request, never became a
log line, and never became a metric. They are absent from every system
you have. The only way to size them is against what the same hour of the
same weekday normally looks like, and that is an estimate with an error
bar, not a measurement.

**Sampled traces cannot count money.** If your business events ride the
same sampling decision as your spans, a 10% sample means every dollar
figure is a 10× extrapolation, and the error bar is invisible in the
number that reaches the postmortem.

Then there is the thing that decides whether any of it matters: if the
number cannot be tied back to the payment provider's ledger, Finance has
no way to check it, and a number Finance cannot check is a number
Finance will not use.

## What shortfall reports instead

One incident window in, four legs and a trust line out — each labelled
by the kind of evidence behind it, because the difference between a
measurement and an estimate is the whole point:

| Leg | What it is | Evidence |
|---|---|---|
| **Realized loss** | Transactions that terminally failed, summed, de-duplicated by entity and net of anything that later succeeded | deterministic |
| **Deferred value** | In-flight and backlogged value, bucketed by age, with the registry's SLA deciding what has become lost | deterministic |
| **Unrealized loss** | Demand that never arrived, sized against a seasonal baseline — always a range | estimate |
| **Customer impact** | Distinct entities, segments, top accounts | deterministic |
| **Coverage ratio** | Of the money your provider's ledger recorded, how much your telemetry also saw | trust |

Coverage is the leg that makes the other four defensible. It is computed
per (flow, currency) against the reconciled ledger and reported as the
**worst** slice, not the average, because a trust number is a
weakest-link number: a silently dropped exporter should show up as a low
figure, not get smoothed away. When it reads 0.62, `shortfall reconcile`
attributes the gap per slice, so you can see which flow and which
currency the missing 38% is in.

## What it is, concretely

A Go library and a CLI. There is no service to run, no datastore to
install, and no agent to deploy.

You attach the business facts where a request enters — flow, entity id,
pre-hashed customer id, the amount in minor units — and call `Record()`
at each stage transition. Each call produces two things: a bounded
`biz_*` metric family for the cheap always-on aggregate view, and one
outcome event carrying the exact amount and the ids, emitted regardless
of any sampling decision. Both ship to the backend you already run
through an export adapter. At incident time the engine reads them back
through a query adapter and computes the legs.

Adapters are the only vendor-aware code, and each one is its own nested
Go module — a Prometheus shop never pulls the AWS SDK, a GCP shop pulls
neither, and nobody pulls stripe-go by accident. If you already run an
OpenTelemetry Collector, or your backend has no adapter of its own, the
OTLP one reaches anything a collector fans out to.

## The opinions, stated up front

These are decisions, not defaults, and they are the reason the output is
worth reconciling. If you disagree with one, you will disagree with the
library.

- **Money is `int64` minor units.** Every amount the library holds,
  propagates, and does arithmetic on is an integer count of minor units,
  and `biz/` is float-free by a gate rule rather than by convention.
  Currencies disagree about how many decimal places that is (USD two,
  JPY zero, BHD three), so `Money` carries its own `Exponent` instead of
  assuming cents. Where a backend imposes floats, the conversion is
  confined to the adapter: a TSDB stores `float64`, so the metric
  exporters and the PromQL querier convert at that boundary and the
  engine owns reading money back out. Statistical code — baselines,
  recovery fractions — uses floats freely and lives outside `biz/` by
  design (ADR-0001).
- **Deterministic and estimated values never merge into one figure.**
  No renderer sums realized loss with unrealized loss. You can add them
  yourself; the library will not do it silently on your behalf.
- **Amounts and ids ride events, never metric labels.** A customer id in
  a label set is an unbounded-cardinality incident waiting to happen, so
  the metric families carry a fixed label vocabulary and nothing else.
- **A leg that cannot be grounded says so.** An events-only backend
  cannot answer the deferred leg. That leg reports `NotAvailable` with a
  reason rather than a confident zero, because a zero is a claim.
- **No severity ladder in the registry means no severity suggestion.**
  The library does not invent a threshold it was not given.
- **PII is fenced in code.** Raw emails, PANs, and IBANs are rejected at
  the `biz.*` boundary rather than discouraged in a style guide.

## Get started

Three questions, in order: how value signals get **emitted** from your
services, how they are **gathered** back into an incident answer, and
what ships in the box for both. The
[Quickstart](docs/quickstart.md) walks the same path hands-on — nothing
to a rendered report in ten minutes, with zero external services.

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

`EntityID` is the field doing the real work. It is what lets four retried
failures collapse into one lost payment at report time, so it should be
the identifier you already treat as the idempotency key.

The context rides W3C Baggage across service hops, so a flow that spans
three services is still one flow. Queue consumers wrap their backlog in
`emit.InFlightTracker`, which publishes the gauge the deferred leg
reads.

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
  same job on message headers.

A worked two-service example (webhook Lambdas → payments-service) lives
in [docs/integration-webhook-lambdas.md](docs/integration-webhook-lambdas.md).

### 2. Gather — ask for the damage

Where step 1 exported decides how you read back.

**Prometheus (metrics) and/or a SQL outcomes table (events).** The CLI
reads both directly:

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

**CloudWatch.** The EMF records from step 1 are already in CloudWatch
Logs; a reporting job reads them back with the `cwinsights` querier,
hands it to `engine.Compute`, and renders the same report. CloudWatch
Logs is an event store, so it grounds realized loss and customer impact;
pair it with a promql-readable metrics store to ground the deferred and
unrealized legs too.

The equivalent ad-hoc scan in Logs Insights (field names verbatim from
the EMF record):

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

**One honesty note covering both query sets.** Those ad-hoc sums are
triage numbers. They are not de-duplicated by entity, and entities that
later succeeded are not excluded — which is to say they have exactly the
retry problem described at the top of this page. `shortfall impact`
applies both corrections on the events path. That is the number Finance
sees; the queries above are the number you eyeball at 3am.

Which backend grounds which leg is a real constraint, not a footnote:
metrics ground the deferred and unrealized legs, events ground realized
de-dup and customer impact, and wiring both signal kinds is what makes
every leg answerable. The full matrix is in
[adapters.md](docs/adapters.md). `shortfall reconcile --ledger` adds the
coverage ratio on top.

### 3. What ships in the box

| Surface | What you get | Where |
|---|---|---|
| Instrument | `ValueContext`, int64 minor-unit `Money`, the PII guard | `biz` |
| Record | `Record` per stage, `InFlightTracker`/`SetInFlight` backlog gauges, `Flush` | `emit` |
| Propagate | HTTP Baggage middleware + egress-fenced Transport; Kafka, SQS, AMQP carriers | `propagate/*` |
| Export | OTLP (metrics + events, to any collector), Prometheus (metrics), CloudWatch EMF (metrics + events), Google Cloud (Cloud Monitoring metrics + Cloud Logging events) | `adapters/export/*` |
| Query back | `promql` (metrics); `cwinsights`, `sql` (events) | `adapters/query/*` |
| Compute & render | four legs + coverage + suggested severity, as text/JSON/markdown | `engine`, `cmd/shortfall` |
| Reconcile | Stripe ledger reconciler feeding the coverage ratio | `adapters/payment/stripe` |
| Notify | impact writers for PagerDuty, incident.io, FireHydrant, Rootly, Slack | `adapters/incident/*` |

Runnable examples live in the `biz`, `emit`, and `engine` packages
(`go doc`, and pkg.go.dev once the repo is public).

## Performance

`Record()` runs inside your request path, so its cost is part of the
contract rather than an implementation detail. Apple M-class laptop, Go
defaults. Every PR is compared against `main` with benchstat and the
delta is written into the CI job summary; that job is advisory today and
becomes a required check once the hot-path baselines stabilise:

| Path | ns/op | allocs/op |
|---|---:|---:|
| `emit.Record` (accepted) | 647 | 3 |
| `biz.vc` encode / decode | 128 / 187 | 3 / 1 |
| In-flight age bucketing | 0.23 | 0 |
| `engine.Compute`, 200k events | 0.75 s | — |

These are single-goroutine figures on an idle machine. Behaviour under
sustained concurrent load is not yet characterised.

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
