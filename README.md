# shortfall

**What an incident cost, who it hit, and how sure you are.**

shortfall is a Go library and CLI that measures the dollar impact of an
incident from telemetry your services already emit, and reports it in a
form Finance can audit. There is no service to run, no datastore to
install, and no agent to deploy.

📖 **[Documentation](https://github.com/NightWatchEng/shortfall/wiki)** —
[Quickstart](https://github.com/NightWatchEng/shortfall/wiki/quickstart) ·
[Integration guide](https://github.com/NightWatchEng/shortfall/wiki/integration) ·
[Backends](https://github.com/NightWatchEng/shortfall/wiki/adapters) ·
[Architecture](https://github.com/NightWatchEng/shortfall/wiki/architecture-README)

## The problem

The postmortem template has a field for customer and revenue impact. It
gets filled by taking a failure count off a dashboard, multiplying by a
remembered average order value, and prefixing a `~`. Finance has nothing
to reconcile that against, so it is filed as an anecdote.

A failure count is not money, for four reasons:

- **A retry is not a second loss.** Four retried captures are one lost
  payment. Collapsing them needs the entity id — which is gone by the
  time a request becomes a metric increment.
- **Delayed money is not lost money.** Backlogged value is at risk, not
  gone. Which part becomes loss is decided by an SLA deadline in a config
  file, not by the telemetry.
- **The largest loss leaves no record.** Checkouts abandoned while the
  payment page timed out never became a request, a log line, or a metric.
  They can only be sized against a baseline, as a range.
- **Sampled traces cannot count money.** At a 10% sample every dollar
  figure is a 10× extrapolation with an invisible error bar.

And if the number cannot be tied back to the payment provider's ledger,
Finance cannot check it, so Finance will not use it.

## What it reports

One incident window in; four legs and a trust line out, each labelled by
the kind of evidence behind it:

| Leg | What it is | Evidence |
|---|---|---|
| **Realized loss** | Terminally failed transactions, summed, de-duplicated by entity, net of anything that later succeeded | deterministic |
| **Deferred value** | In-flight and backlogged value by age bucket, with the registry's SLA deciding what has become lost | deterministic |
| **Unrealized loss** | Demand that never arrived, sized against a seasonal baseline — always a range | estimate |
| **Customer impact** | Distinct affected accounts, a per-segment breakdown, and the top accounts by failed value | deterministic |
| **Coverage ratio** | Of the money the provider's ledger recorded, how much your telemetry also saw | trust |

Coverage is what makes the other four defensible. It is computed per
(flow, currency) and reported as the **worst** slice, not the average — a
trust number is a weakest-link number. When it reads 0.62,
`shortfall reconcile` attributes the missing 38% to a flow and a currency.

Deterministic and estimated legs are never summed into one figure.

**Why "shortfall".** It is the finance word for exactly this — the gap
between expected and actual — and it carries the nuance the report is
built around: a shortfall can be recovered. Deferred value is the case in
point. It is money that is late, not gone, and it becomes loss only when
a deadline in the registry says so. A word meaning only *loss* would have
prejudged it.

## Install

```sh
go get github.com/NightWatchEng/shortfall
```

**Not yet:** the repository is private, so the checksum database cannot
verify the module and `go get` fails. Until the flip, clone it and use a
`replace` directive — the [quickstart](docs/quickstart.md) has the exact
steps.

The core module pulls `otel` and `yaml.v3` and nothing else. Every
adapter under `adapters/` is a separate nested module, so a Prometheus
shop never compiles the AWS SDK and nobody compiles stripe-go by
accident.

## Instrument

Attach the business facts where a request enters, then call `Record` at
each stage transition:

```go
func main() {
    ctx := context.Background()

    reg, err := registry.Load("registry.yaml") // the flow registry Finance co-signs
    if err != nil {
        log.Fatal(err)
    }

    exp, err := otlp.New(ctx) // to your collector; reads OTEL_EXPORTER_OTLP_*
    if err != nil {
        log.Fatal(err)
    }

    em, err := emit.New(&reg, exp)
    if err != nil {
        log.Fatal(err)
    }
    defer em.Close(ctx)

    http.HandleFunc("POST /invoices/{invoice}/pay", func(w http.ResponseWriter, r *http.Request) {
        ctx, err := biz.WithValueContext(r.Context(), biz.ValueContext{
            Flow:       "invoice.pay",
            EntityID:   r.PathValue("invoice"),         // idempotency key: retries de-dup on it
            CustomerID: r.Header.Get("X-Account-Hash"), // pre-hashed; raw ids are rejected
            Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
            Kind:       biz.KindFee,
        })
        if err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            return
        }

        em.Record(ctx, "auth", biz.ResultSuccess) // once per stage transition
        w.WriteHeader(http.StatusAccepted)
    })

    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Each `Record` call produces two things: a bounded `biz_*` metric point
for the always-on aggregate view, and one outcome event carrying the
exact amount and the ids, emitted regardless of any sampling decision.
The value context rides W3C Baggage across service hops, so a flow
spanning three services is still one flow.

OTLP is the vendor-neutral path: one integration, and your collector fans
the signals out to whatever you already run. Swap it for
`adapters/export/{prometheus,cloudwatch,gcp}` to write a backend directly.

## Report

```sh
shortfall impact --registry registry.yaml \
  --from 2026-08-28T14:00:00Z --to 2026-08-28T15:30:00Z \
  --flow invoice.pay --format markdown \
  --prometheus http://prometheus:9090 --sql "file:outcomes.db"
```

Metrics ground the deferred and unrealized legs; events ground realized
de-duplication and customer impact. Wiring both signal kinds is what
makes every leg answerable — see [Backends](docs/adapters.md) for the
matrix. `shortfall reconcile --ledger` adds the coverage ratio.

`--prometheus` is what the wiring above feeds: the collector writes the
`biz_*` families to Prometheus. `--sql` is the events half, and it reads
the fixed `biz_outcomes` table in [backends](docs/adapters.md) — landing
OTLP log records in that shape is a mapping you own, not something a
collector does for you. Export straight to CloudWatch or Cloud Logging
instead and you skip the CLI entirely, reading back through that
backend's query adapter from a small reporting job — same
`engine.Compute`, same report. The
[worked example](docs/example-webhooks.md) shows it end to end.

## Design rules

These are decisions, not defaults. If you disagree with one, you will
disagree with the library.

- **Money is `int64` minor units.** `Money` carries its own `Exponent`,
  because currencies disagree about decimal places. `biz/` is float-free
  by a gate rule rather than by convention; floats are confined to
  adapters and to statistical code (ADR-0001).
- **Deterministic and estimated values never merge.** No renderer sums
  realized loss with unrealized loss.
- **Amounts and ids ride events, never metric labels.** Metric families
  carry a fixed label vocabulary; a customer id in a label set is an
  unbounded-cardinality incident waiting to happen (ADR-0004).
- **A leg that cannot be grounded says so.** On an events-only backend
  the deferred leg comes back marked unavailable, with a caveat naming
  why, because a zero is a claim (ADR-0017).
- **No severity ladder in the registry means no severity suggestion.**
- **PII is fenced in code.** Raw emails, PANs and IBANs are rejected at
  the `biz.*` boundary, not discouraged in a style guide.

## Performance

`Record` runs inside your request path, so its cost is part of the
contract: 2.3 µs per accepted outcome on one core, 1.0 µs on eight, with
a throughput ceiling around 950k outcomes/s that it does **not** scale
past. A slow backend never reaches the caller as latency — it costs
dropped outcomes instead, and they are counted.

[Performance](docs/performance.md) carries the methodology, the scaling
curves and an explicit statement of what was not measured. Read it before
sizing anything.

## Documentation

Everything below is on the
**[wiki](https://github.com/NightWatchEng/shortfall/wiki)**, which is the
readable surface — one page per topic, in reading order, regenerated from
`docs/` on every push to main. The in-repo links here are the same pages
at their source of truth.

**Start here**
- [Quickstart](docs/quickstart.md) — instrument a service and watch `biz_*` come out, in 10 minutes, no external services.
- [Integration guide](docs/integration.md) — the step-by-step for wiring your own service.
- [Worked example](docs/example-webhooks.md) — webhook Lambdas → payments-service, end to end.

**Reference**
- [Backends & adapters](docs/adapters.md) — which backend grounds which leg.
- [Registry](docs/registry.md) — every field of the flow registry.
- [Money & the legs](docs/money.md) — what a "dollar" means here, for Finance.
- [Performance](docs/performance.md) — scaling numbers and where they stop applying.

**Specification**
- [Portability contract](docs/portability.md) — what another implementation, or an external adapter, must satisfy.

**Design**
- [Architecture](docs/architecture/README.md) — C4 levels 1–3 and the money-path sequences.
- [Decision records](docs/adr/README.md) — one ADR per irreversible choice.

API reference: `go doc`, plus the runnable examples in the `biz`, `emit`
and `engine` packages — and pkg.go.dev once the repo is public.

## Status

Pre-release, under active construction. Nothing here is stable yet.

**Versioning:** v0.x — the public interfaces (`biz`, `registry`, `emit`,
`engine`, `query`, `propagate/*`) may change between minor versions.
v1.0.0 will be tagged only after those interfaces have survived two
external adapters not written by the authors.

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Use, modify and
redistribute it commercially or privately without asking; when you
redistribute, keep the notices, ship the [LICENSE](LICENSE) and
[NOTICE](NOTICE), and mark changed files as changed.
