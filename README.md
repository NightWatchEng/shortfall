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
  (OTLP, Prometheus, CloudWatch EMF, Datadog, StatsD, Splunk HEC, Loki)
  and reports read back through query adapters — no new datastore, no
  agent, no service to run.
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

```sh
go build -o shortfall ./cmd/shortfall
./shortfall validate registry/testdata/registry.yaml
./shortfall impact --registry registry/testdata/registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z --sql "file:demo.db" --sql-driver sqlite
```

The third command wants a few outcome rows to read — the
[Quickstart](docs/quickstart.md) walks from nothing to a rendered report
in 10 minutes with zero external services. In your own services,
instrumentation is:

```go
ctx, err := biz.WithValueContext(r.Context(), biz.ValueContext{
    Flow:       "invoice.pay",
    EntityID:   invoiceID,
    CustomerID: hashedAccountID, // pre-hashed — raw ids never enter biz.*
    Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
    Kind:       biz.KindFee,
})
// ...
em.Record(ctx, "auth", biz.ResultSuccess) // once per stage transition
```

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
