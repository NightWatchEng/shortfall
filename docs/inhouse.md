# Instrumenting an in-house payment API

This is the paved road for measuring incident dollar-impact when **you own
the payment code** — an internal payments service, a billing worker, an
orders API. Unlike a third-party provider (Stripe, Adyen), you can call the
library directly at each stage, so there is no webhook receiver to build and
no wrapped SDK: just the M3 primitives — a registry, an emitter, ingress
stamping, per-stage `Record`, in-flight tracking, and context propagation.

The running example is the **`invoice.pay`** flow with three stages —
`auth` → `capture` → `settle` — the same shape the
[`examples/checkout`](../examples/checkout) simulator models (segments
`smb`/`enterprise`, currency `USD`, pre-hashed customer ids like `h:c000042`,
entity ids like `inv_00000001`). Note that `examples/checkout` is a *pure
simulator*: it generates ground-truth ledgers and does **not** itself call
`emit` or `httpmw`. This recipe shows how you would instrument a real service
of that shape; every symbol below exists in the library today.

> Money is always **minor units** (`14900` = `$149.00` at exponent 2) and
> never a float. Amounts and ids ride on *events*; metrics carry only bounded
> labels (ADR-0004). The library enforces both — you cannot accidentally put
> an amount on a metric label.

---

## 1. Declare the flow in a registry

The registry is the one place Finance reviews. It names your flows, their
stages, the money kind, SLAs, and the estimator used when an amount is not
yet known. Mirror [`registry/testdata/registry.yaml`](../registry/testdata/registry.yaml):

```yaml
version: 1
segments: [smb, enterprise]
propagation:
  allow_hosts: ["*.internal.example.com", "api.example.com"]
flows:
  invoice.pay:
    money: { kind: fee }            # gmv | net_revenue | fee | take_rate
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
      - { name: settle,  signals: ["queue:settle.q"] }
    sla:
      capture: { deadline: PT30M, on_breach: lost }     # ISO-8601 durations
      settle:  { deadline: P1D,  on_breach: at_risk }
    estimator: { default_minor: 18750, by_segment: { smb: 14200, enterprise: 91000 } }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 8, holidays: us }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.6, within: PT2H }
    reconcile: { source: "sql:ledger.payments" }
```

Durations here are **ISO-8601** (`PT30M`, `P1D`, `PT2H`) — not Go duration
strings. The validator rejects unknown fields, and only accepts
`baseline.seasonality: hour_of_week`, `recovery.model: usage_loss_curve`, and
a `sql:`/`stripe:` reconcile source.

Load it once at startup:

<!-- docsnippets:continues -->
```go
reg, err := registry.Load("registry.yaml")   // returns a registry.Registry value
if err != nil {                               // (registry.Parse(rawBytes) for in-memory bytes)
    log.Fatalf("registry: %v", err)
}
```

`Load`/`Parse` return a `registry.Registry` **value**; the emitter and
middleware constructors below take a `*registry.Registry`, so pass `&reg`.

---

## 2. Wire the emitter

The emitter turns stage transitions into bounded metrics and unsampled
outcome events. It takes a validated registry and an **exporter** — pick any
adapter under [`adapters/export/`](../adapters/export) (each is its own
nested module, so you only pull the backend you use). Using Prometheus:

```go
import (
    "github.com/NightWatchEng/shortfall/emit"
    prom "github.com/NightWatchEng/shortfall/adapters/export/prometheus"
)

exp, err := prom.New()               // serve exp.Gatherer() on /metrics
if err != nil {
    log.Fatal(err)
}
em, err := emit.New(&reg, exp)       // emit.New(*registry.Registry, exporter, ...EmitterOption)
if err != nil {
    log.Fatal(err)
}
defer em.Close(context.Background()) // final flush + exporter shutdown; idempotent
```

Useful `EmitterOption`s: `emit.WithFlushInterval(d)` (background flush cadence;
`0` means *you* drive `em.Flush(ctx)`), `emit.WithBufferSize(n)` (default
10000), `emit.WithLogger(l)`, `emit.WithClock(now)`.

`em` satisfies the `emit.Emitter` interface:

<!-- docsnippets:reference -->
```go
Record(ctx context.Context, stage string, result biz.Result, opts ...Option)
SetInFlight(flow, stage, ageBucket string, money biz.Money, count int64)
RecordProviderCall(provider, op, outcome string)
```

`Record` never blocks and returns nothing — it cannot fail your request path.

---

## 3. Stamp the ValueContext at ingress

`Record` reads the `biz.ValueContext` (flow, amount, ids, segment) from the
request `context.Context`. Something has to put it there at the flow's entry
point. For an HTTP entry point, wrap your handler with the middleware and give
it an ingress hook:

```go
import (
    "github.com/NightWatchEng/shortfall/biz"
    "github.com/NightWatchEng/shortfall/propagate/httpmw"
)

ingress := func(r *http.Request) (biz.ValueContext, bool) {
    if r.Method != http.MethodPost || r.URL.Path != "/pay" {
        return biz.ValueContext{}, false // not a flow entry point
    }
    inv := parseInvoice(r) // your decode
    return biz.ValueContext{
        Flow:       "invoice.pay",
        EntityID:   inv.ID,                 // "inv_00000001" — events only
        CustomerID: inv.HashedCustomer,     // "h:c000042" — PRE-HASHED, see below
        Segment:    inv.Segment,            // "smb" | "enterprise"
        Money:      biz.Money{Amount: inv.AmountMinor, Currency: "USD", Exponent: 2},
        Kind:       biz.KindFee,
    }, true
}

handler = httpmw.Middleware(&reg, httpmw.WithIngress(ingress))(handler)
```

The middleware first tries to recover a `biz.vc` that an upstream service
already propagated; only if there is none does it call your ingress hook. The
stamped context flows to every `Record` call downstream of the handler.

> **CustomerID must arrive pre-hashed.** The library has no hashing helper on
> purpose — hash the account id in your own code before you build the
> `ValueContext`. `biz.CheckPII(field, s)` is a *guard* that rejects raw PANs
> (Luhn), emails, and IBANs; it does not hash. `EntityID`/`CustomerID` are
> events-only and never reach a metric label.

If a stage runs outside an HTTP handler (a queue consumer, a cron), stamp the
context yourself:

<!-- docsnippets:continues -->
```go
ctx, err := biz.WithValueContext(ctx, vc)   // err is *biz.OversizeError if vc encodes > 512 bytes
```

---

## 4. Record each stage outcome

Call `Record` once per stage transition with the terminal result for that
stage. Amounts and currency come from the context; you pass the stage name and
result:

```go
// auth (synchronous): succeeded
em.Record(ctx, "auth", biz.ResultSuccess)

// auth failed with a provider 5xx
em.Record(ctx, "auth", biz.ResultFailed,
    emit.WithSource("payments-api"),
    emit.WithErr("gateway 502"))

// capture is async — the txn is deferred until the queue works it
em.Record(ctx, "capture", biz.ResultDeferred)

// user abandoned before auth completed (telemetry may never see this;
// record it where you can detect it)
em.Record(ctx, "auth", biz.ResultAbandoned)
```

Results (`biz.Result`): `ResultSuccess`, `ResultFailed`, `ResultDeferred`,
`ResultAbandoned`, `ResultUnknown`. Options:

- `emit.WithSource(s)` — where the outcome was observed, e.g. `"payments-api"`.
- `emit.WithErr(s)` — a short failure description (≤512 bytes).
- `emit.WithAt(t)` — **override the event time.** Use the provider's/event's
  own timestamp for anything that can arrive late (a delayed queue message, a
  replayed event); receipt-time stamping would move realized loss into the
  wrong incident window.

---

## 5. Track in-flight (deferred) value

Deferred value — money sitting in the `capture` and `settle` queues during an
incident — is a *level*, not a count. Use the `InFlightTracker`: `Track` when
a transaction enters a queue, `Done` when it leaves, and `Publish` (or a
background `Start`) to emit the current levels bucketed by age.

```go
tr := emit.NewInFlightTracker(em)
tr.Start(15 * time.Second)   // publish gauge levels every 15s
defer tr.Close()

// on enqueue:
tr.Track("invoice.pay", "capture", txn.ID, money, time.Now())
// on dequeue / completion:
tr.Done("invoice.pay", "capture", txn.ID)
```

`Publish` computes each item's age, buckets it (`lt1m`, `1m-5m`, `5m-30m`,
`30m-2h`, `gt2h`), and calls `em.SetInFlight` per (flow, stage, bucket). The
tracker is bounded (`emit.WithTrackerMaxItems`, default `1<<20`); check
`tr.Overflowed()` / `tr.Rejected()` if you run at very high concurrency.

---

## 6. Propagate the ValueContext across services

When a stage crosses a process boundary, carry the `biz.vc` so the next
service does not have to re-derive it.

**Outbound HTTP** — wrap your client's transport with the egress fence. It
injects `biz.vc` only toward hosts on the registry's `propagation.allow_hosts`
allowlist and strips it (fails closed) everywhere else, so amounts never leak
to third parties:

<!-- docsnippets:continues -->
```go
client := &http.Client{
    Transport: httpmw.NewTransport(&reg, http.DefaultTransport),
}
```

**Queues** — inject on the producer, extract on the consumer, using the
carrier for your transport (no transport SDK is imported by the library):

<!-- docsnippets:continues -->
```go
// producer (Kafka): the carrier wraps a *[]kafka.Header (the library's own
// header type); map it to/from your Kafka client's headers at the edge.
import (
    "github.com/NightWatchEng/shortfall/propagate"
    "github.com/NightWatchEng/shortfall/propagate/kafka"
)
var hdrs []kafka.Header
if err := propagate.Inject(kafka.NewCarrier(&hdrs), vc); err != nil { /* handle */ }
// ... copy hdrs onto the outgoing message ...

// consumer: build the carrier over the received headers, then extract.
vc, ok, err := propagate.Extract(kafka.NewCarrier(&hdrs))
if err != nil { /* malformed baggage */ }
if ok {
    ctx, _ = biz.WithValueContext(ctx, vc)
    em.Record(ctx, "capture", biz.ResultSuccess)
}
```

Carriers for the other transports are `sqs.NewCarrier(attrs)` and
`amqp.NewCarrier(table)`; all implement `propagate.Carrier`.

---

## 7. What you should see

With the above wired, a capture-queue stall incident produces:

- `biz_txn_total{flow=invoice.pay,stage=auth,outcome=failed,...}` climbing,
- `biz_value_total{...}` accumulating the failed fee value (realized loss),
- `biz_inflight_value{flow=invoice.pay,stage=capture,age_bucket=5m-30m,...}`
  rising as capture backs up (deferred value),
- and one `biz.outcome` event per transition carrying the amount, entity id,
  and (pre-hashed) customer id for the customers leg.

The engine's `shortfall impact` (M6) turns those into the realized / deferred
/ customers ledger block. Nothing in this recipe samples events, so the dollar
figures are complete regardless of trace sampling.

---

## Checklist

- [ ] Registry entry for the flow, reviewed by Finance, loaded at startup.
- [ ] Emitter constructed with an exporter; `em.Close` deferred.
- [ ] Ingress hook (or `biz.WithValueContext`) stamps the flow entry point.
- [ ] `em.Record` at every stage transition, with `WithAt` for late events.
- [ ] `InFlightTracker` for the async (queued) stages.
- [ ] Egress fence on outbound clients; carriers on queue producers/consumers.
- [ ] Customer ids pre-hashed before they reach a `ValueContext`.
