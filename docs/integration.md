# Integration guide

How to wire shortfall into a service you own, step by step. At the end
an incident in that service produces a four-leg impact report instead of
a guess.

Work through it in order — each step depends on the one before it. The
running example is an `invoice.pay` flow with three stages,
`auth → capture → settle`. If your payment path is a third-party
provider rather than your own code, the same steps apply; the
[worked example](example-webhooks.md) shows the two-service shape.

**Before you start:** money is always **minor units** (`14900` = $149.00
at exponent 2) and never a float. Amounts and ids ride *events*; metrics
carry only bounded labels. The library enforces both — you cannot
accidentally put an amount on a metric label.

---

## Step 1 — Model the flow

shortfall does not model services. It models **flows with ordered
stages**, and a flow may span any number of processes. Before writing
code, answer four questions:

| Question | What it decides |
|---|---|
| What is the unit of business value? | the flow (`invoice.pay`) and its money `kind` |
| What are its stages, in order? | `stages[0]` is the entry stage — the counterfactual leg is measured there |
| What identifier survives a retry? | `EntityID`, the key realized loss de-duplicates on |
| When does a delay become a loss? | the per-stage SLA deadline and its `on_breach` rule |

Get `EntityID` right and everything else is recoverable. It should be
the identifier you already treat as the idempotency key — an invoice id,
an order id, a payment intent id. It is what collapses four retried
failures into one lost payment at report time.

---

## Step 2 — Declare the flow in a registry

The registry is the one file Finance reviews. It names your flows, their
stages, the money kind, SLAs, and the estimator used when an amount is
not yet known.

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

Durations are ISO-8601 (`PT30M`, `P1D`), not Go duration strings.
Unknown fields are rejected, so a typo fails rather than silently
defaulting. Check it before you ship it:

```sh
shortfall validate registry.yaml
```

Every field is documented in the [registry reference](registry.md).
Load it once at startup:

<!-- docsnippets:continues -->
```go
reg, err := registry.Load("registry.yaml")   // returns a registry.Registry value
if err != nil {                               // (registry.Parse(rawBytes) for in-memory bytes)
    log.Fatalf("registry: %v", err)
}
```

`Load` and `Parse` return a `registry.Registry` **value**; the emitter
and middleware constructors take a `*registry.Registry`, so pass `&reg`.

---

## Step 3 — Wire the emitter

The emitter turns stage transitions into bounded metrics and unsampled
outcome events. It takes a validated registry and an **exporter** — pick
the adapter for the backend you already run. Each is its own nested
module, so you compile only the one you use. Using Prometheus:

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

Options worth knowing: `emit.WithFlushInterval(d)` sets the background
flush cadence (`0` means *you* drive `em.Flush(ctx)` — the right choice
in a Lambda, which freezes between invocations); `emit.WithBufferSize(n)`
defaults to 10000; `emit.WithLogger(l)` and `emit.WithClock(now)` exist
for tests.

`em` satisfies the `emit.Emitter` interface:

<!-- docsnippets:reference -->
```go
Record(ctx context.Context, stage string, result biz.Result, opts ...Option)
SetInFlight(flow, stage, ageBucket string, money biz.Money, count int64)
RecordProviderCall(provider, op, outcome string)
```

`Record` never blocks and returns nothing — it cannot fail your request
path. Its failure modes are visible as `biz_dropped_events_total{reason}`
instead.

Which exporter to pick, and what each one grounds, is in
[backends & adapters](adapters.md). If you already run an OpenTelemetry
Collector, `adapters/export/otlp` reaches anything it fans out to.

---

## Step 4 — Stamp the value context at ingress

`Record` reads the `biz.ValueContext` — flow, amount, ids, segment —
off the request `context.Context`. Something has to put it there at the
flow's entry point. For an HTTP entry point, wrap the handler and give
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

The middleware first tries to recover a `biz.vc` that an upstream
service already propagated; only if there is none does it call your
hook. The stamped context then flows to every `Record` call downstream.

> **`CustomerID` must arrive pre-hashed.** There is no hashing helper,
> on purpose — hash the account id in your own code before building the
> `ValueContext`. `biz.CheckPII(field, s)` is a *guard*: it rejects raw
> PANs (Luhn), emails and IBANs. It does not hash.

If a stage runs outside an HTTP handler — a queue consumer, a cron —
stamp the context yourself:

<!-- docsnippets:continues -->
```go
ctx, err := biz.WithValueContext(ctx, vc)   // err is *biz.OversizeError if vc encodes > 512 bytes
```

---

## Step 5 — Record each stage transition

Call `Record` once per stage transition, with that stage's terminal
result. Amounts and currency come from the context; you pass the stage
name and the result:

```go
// auth (synchronous): succeeded
em.Record(ctx, "auth", biz.ResultSuccess)

// auth failed with a provider 5xx
em.Record(ctx, "auth", biz.ResultFailed,
    emit.WithSource("payments-api"),
    emit.WithErr("gateway 502"))

// capture is async — the txn is deferred until the queue works it
em.Record(ctx, "capture", biz.ResultDeferred)

// user abandoned before auth completed (record it where you can detect it)
em.Record(ctx, "auth", biz.ResultAbandoned)
```

Results are `ResultSuccess`, `ResultFailed`, `ResultDeferred`,
`ResultAbandoned` and `ResultUnknown`. Three options matter:

- `emit.WithSource(s)` — where the outcome was observed, e.g. `"payments-api"`.
- `emit.WithErr(s)` — a short failure description (≤512 bytes).
- `emit.WithAt(t)` — **override the event time.** Use the event's own
  timestamp for anything that can arrive late (a delayed queue message,
  a replayed provider webhook). Receipt-time stamping would move
  realized loss into the wrong incident window, which is the one thing
  that makes a post-incident number unreconcilable.

---

## Step 6 — Track in-flight value on queued stages

Deferred value — money sitting in a queue during an incident — is a
*level*, not a count, and the deferred leg reads it from the
`biz_inflight_value` gauge rather than from `deferred` outcome events.
Recording `ResultDeferred` marks the outcome; the tracker is what the
leg actually measures. Skip this step and the deferred leg has nothing
to stand on.

```go
tr := emit.NewInFlightTracker(em)
tr.Start(15 * time.Second)   // publish gauge levels every 15s
defer tr.Close()

// on enqueue:
tr.Track("invoice.pay", "capture", txn.ID, money, time.Now())
// on dequeue / completion:
tr.Done("invoice.pay", "capture", txn.ID)
```

`Publish` ages each tracked item, buckets it (`lt1m`, `1m-5m`, `5m-30m`,
`30m-2h`, `gt2h`) and calls `em.SetInFlight` per (flow, stage, bucket).
The tracker is bounded — `emit.WithTrackerMaxItems`, default `1<<20` —
so check `tr.Overflowed()` and `tr.Rejected()` if you run at very high
concurrency.

---

## Step 7 — Propagate across process boundaries

When a stage crosses a process boundary, carry the `biz.vc` so the next
service does not have to re-derive it. One header, one Baggage member.

**Outbound HTTP.** Wrap your client's transport. It injects `biz.vc`
only toward hosts on the registry's `propagation.allow_hosts` allowlist
and strips it everywhere else, so amounts never leak to a third party.
This is deny-by-default: an empty allowlist denies everything, and a
`biz.vc` that some other propagator added is deleted rather than merely
not added.

<!-- docsnippets:continues -->
```go
client := &http.Client{
    Transport: httpmw.NewTransport(&reg, http.DefaultTransport),
}
```

Route both your internal calls and your provider calls through this one
client. Internal hosts you allowlisted keep the context, so the
downstream service's `Record` calls land on the same entity and dollars
— one flow, measured across every hop. Your payment provider is not
allowlisted, so its requests leave clean.

> **The fence is this Transport, so only two things escape it.**
>
> *A request that never goes through it.* Route every outbound call
> through a client built this way, including calls made inside SDKs — most
> accept a custom `http.Client`.
>
> *An injector composed inside it.* `RoundTrip` rebuilds the header and
> then hands the request to its `base`, so a transport that injects
> baggage must wrap this one from the **outside**:
>
> ```
> otelhttp.NewTransport(httpmw.NewTransport(&reg, base))  // safe
> httpmw.NewTransport(&reg, otelhttp.NewTransport(base))  // re-injects
> ```
>
> A globally-installed generic Baggage propagator is the usual injector in
> both cases — `otelhttp` reaches for `otel.GetTextMapPropagator()` by
> default — but it is not itself the hazard. Everything that does reach
> the fence is fenced: toward a non-allowlisted host a `biz.vc` is deleted
> even when a global propagator put it there (ADR-0003). Shipping
> transaction amounts to a third party is a decision someone makes on
> purpose.

**Queues.** Inject on the producer, extract on the consumer. The
carriers import no queue client library, so your broker client stays
yours:

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

`sqs.NewCarrier(attrs)` and `amqp.NewCarrier(table)` do the same job for
those transports. `Extract` returns three distinguishable outcomes —
present, absent, and present-but-corrupt — and the library counts none
of them; wiring the corrupt case to a counter is your consumer's job.

---

## Step 8 — Check that it worked

With the above wired, a capture-queue stall should show up as:

- `biz_txn_total{flow="invoice.pay",stage="auth",outcome="failed"}` climbing,
- `biz_value_total{...}` accumulating the failed fee value (realized loss),
- `biz_inflight_value{flow="invoice.pay",stage="capture",age_bucket="5m-30m"}`
  rising as capture backs up (deferred value),
- one `biz.outcome` event per transition, carrying the amount, the
  entity id and the hashed customer id (the customers leg).

Then ask for the report the way the [quickstart](quickstart.md) does,
pointed at your own backend. Nothing here samples events, so the dollar
figures are complete regardless of your trace sample rate.

---

## Checklist

- [ ] Flow, stages and `EntityID` chosen; entry stage is `stages[0]`.
- [ ] Registry entry reviewed by Finance, validated, loaded at startup.
- [ ] Emitter constructed with an exporter; `em.Close` deferred.
- [ ] Ingress hook (or `biz.WithValueContext`) stamps the entry point.
- [ ] `em.Record` at every stage transition, with `WithAt` for late events.
- [ ] `InFlightTracker` on every queued stage.
- [ ] Egress fence on outbound clients; carriers on queue producers and consumers.
- [ ] Customer ids pre-hashed before they reach a `ValueContext`.
- [ ] A query adapter for each signal kind you need — see [backends](adapters.md).
