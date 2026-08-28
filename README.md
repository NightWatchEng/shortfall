# shortfall

**What an incident cost, who it hit, and how sure you are.**

A vendor-neutral Go library and reference engine for incident dollar impact.
For any incident window and scope, shortfall answers with four numbers, each
labelled by the kind of evidence behind it:

| Leg | What it is | Evidence |
|---|---|---|
| **Realized loss** | Transactions that failed, summed, de-duplicated by entity | deterministic |
| **Deferred value** | In-flight or backlogged value, by age, with SLA conversion to lost | deterministic |
| **Unrealized loss** | Value that never happened, from a seasonal baseline — always a range | estimate |
| **Customer impact** | Distinct entities, segments, top accounts | deterministic |

plus a **coverage ratio** — telemetry sums reconciled against the ledger —
because a number Finance cannot audit is a number Finance will not use.

## The two questions

"Dollar impact of an incident" conflates two different questions, which is why
it feels unsolvable:

- **Q1 — Attribution (deterministic).** Which specific transactions and
  customers were affected, and what were they worth? Needs per-transaction
  business context attached to the failing telemetry. Auditable. Required for
  refunds, SLA credits, and calling the top-20 accounts.
- **Q2 — Counterfactual (statistical).** How much value did not happen because
  of the degradation? The lost transactions never existed, so no correlation
  id will ever find them. Only a baseline forecast minus actuals can answer
  it, with error bars.

A tool that only does Q1 is silent for upstream outages. A tool that only does
Q2 cannot tell you who to refund. shortfall does both and labels which is
which.

## How it works

Four layers; only the top one knows your vendors:

1. **Capture** — `biz.*` OpenTelemetry attributes, `ValueContext` propagation
   (W3C Baggage over HTTP, headers over queues), and unsampled outcome events:
   money accounting never depends on trace sampling.
2. **Flow registry** — versioned YAML, co-signed by Finance once: what counts
   as money, where the stages live, when deferred becomes lost, what an
   unknown amount is worth, how much demand returns after recovery.
3. **Export adapters** — OTLP by default; Prometheus, StatsD, CloudWatch EMF,
   Splunk HEC, Datadog, Loki natively.
4. **Impact engine** — query-time, over query adapters (PromQL, SQL, LogQL,
   CloudWatch Insights, SPL). The engine only ever asks a backend for
   sum, count, group-by, and time range — so nothing vendor-specific leaks
   past the adapter boundary.

Design invariants, enforced in review and by the library itself:

- Money is `int64` minor units + currency + exponent. Never float.
- Amounts and entity ids ride on **events only**; metrics carry sums with a
  fixed, bounded label set. Cardinality protection is a library guarantee.
- No PAN or PII ever appears in `biz.*` attributes (guarded, not promised).
- Realized and estimated value are never merged into one headline number.

## Getting started

```sh
go get github.com/NightWatchEng/shortfall          # core: biz, emit, engine, registry, query
go get github.com/NightWatchEng/shortfall/adapters/export/prometheus  # adapters are separate modules
```

Attach business context where a request enters, record every stage
transition, and the library does the rest — bounded metrics, unsampled
outcome events, cardinality fences:

```go
ctx, err := biz.WithValueContext(r.Context(), biz.ValueContext{
    Flow:       "invoice.pay",
    EntityID:   invoiceID,
    CustomerID: hashedAccountID, // pre-hashed — raw ids never enter biz.*
    Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
    Kind:       biz.KindFee,
})
// ...
em.Record(ctx, "auth", biz.ResultSuccess)     // once per stage transition
em.Record(ctx, "capture", biz.ResultFailed)
```

At incident time, ask the engine for the report over any window and scope
— from your own code or the CLI:

```go
report, err := engine.Compute(ctx, &reg, querier, engine.Request{
    Window: query.TimeRange{From: incidentStart, To: incidentEnd},
    Flows:  []string{"invoice.pay"},
})
```

```sh
shortfall impact --registry registry.yaml --prometheus http://prom:9090 \
  --from 2026-08-25T09:00:00Z --to 2026-08-25T12:00:00Z
```

Runnable versions of these snippets live in the package examples
(`biz`, `emit`, `engine` on pkg.go.dev); the full path from `go get` to a
rendered report is the [Quickstart](docs/quickstart.md).

## Documentation

- [Quickstart](docs/quickstart.md) — `go get` to a rendered impact report in 10 minutes.
- [Adapters & capability matrix](docs/adapters.md) — which backend grounds which leg.
- [Registry reference](docs/registry.md) — every field of the flow registry.
- [What is a "dollar" here](docs/money.md) — kind semantics, lost vs delayed, why ranges (for Finance).
- [Semantic conventions (draft)](docs/semconv.md) — the `biz.*` attribute and metric shapes.

## Layout

One git repo, multiple Go modules: the core module has no heavy dependencies;
every adapter under `adapters/` is a nested module, so depending on the
Prometheus exporter never pulls a payments SDK into your build.

```
biz/          value types: Money, ValueContext, Outcome
registry/     the YAML flow registry: schema, loader, validation
emit/         stage transitions -> bounded metrics + outcome events
propagate/    HTTP middleware and queue header carriers for ValueContext
engine/       the four legs, baseline, report renderers
query/        the query AST and Querier boundary
cmd/shortfall CLI: validate, impact, reconcile, simulate
adapters/     export, query, payment, incident — each its own module
examples/     synthetic checkout app used as the ground-truth harness
testkit/      scenario runner and exporter conformance suite
docs/adr/     one ADR per design decision
```

## Architecture

C4 diagrams and the three money-path sequences live in
[docs/architecture](docs/architecture/README.md), rendered natively by
GitHub. Decisions live in [docs/adr](docs/adr/README.md).

## Status

Pre-release, under active construction. Nothing here is stable yet.

**Versioning policy:** v0.x — the public interfaces (`biz`, `registry`,
`emit`, `engine`, `query`, `propagate/*`) may change between minor versions.
v1.0.0 will be tagged only after those interfaces have survived two external
adapters not written by the authors.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
