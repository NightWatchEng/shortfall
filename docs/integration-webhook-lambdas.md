# Example integration — webhook Lambdas → payments-service

A common two-system shape: stateless **webhook Lambdas** ingest provider
webhooks (Stripe, Adyen, …) and call a **payments-service** API to process
each one. This walks through instrumenting that pair so that when either
side goes down, `shortfall impact` answers with the four legs — and shows
which leg covers which failure direction.

Shortfall does not model services; it models **flows with ordered
stages**. Here the flow spans both systems: the Lambda owns the entry
stage, payments-service owns the rest.

## 1. Registry

```yaml
version: 1
segments: [smb, enterprise]
propagation:
  allow_hosts:
    - "payments.internal.example.com"   # context egress is deny-by-default (ADR-0003)
severity:
  - { sev: SEV1, min_per_minute: 100000 }
  - { sev: SEV2, min_per_minute: 10000 }
flows:
  payment.webhook:
    money: { kind: fee }
    currencies: [USD]
    stages:                      # stages[0] is the ENTRY stage — the Lambda
      - { name: ingest,  signals: ["webhook:payment_intent.succeeded"] }
      - { name: process, signals: ["http:POST /internal/webhooks/process"] }
    sla:
      process: { deadline: PT30M, on_breach: lost }  # backlog >30m projects to loss
    estimator:                   # value when the payload carries no amount
      default_minor: 18750
      by_segment: { smb: 14200, enterprise: 91000 }
    baseline:
      seasonality: hour_of_week
      lookback_weeks: 8
    recovery:                    # providers redeliver webhooks — most value returns
      model: usage_loss_curve
      recovered_fraction: 0.9
      within: PT2H
```

Two choices worth making deliberately:

- **`stages[0]` is the Lambda's ingest stage.** The counterfactual leg
  counts entries there, so "the Lambdas themselves are down" is measured
  against this stage's baseline.
- **`recovery.recovered_fraction` is high** because webhook providers
  retry delivery; tune it to your provider's redelivery policy.

## 2. The Lambda (entry stage)

Attach value context when the webhook arrives, record `ingest`, and make
the outbound call through the propagating Transport so payments-service
sees the same entity and dollars:

```go
reg, _ := registry.Load("registry.yaml")
exporter := cloudwatch.New() // EMF to stdout — the Lambda-native path
em, _ := emit.New(&reg, exporter)

// One client for the Lambda's lifetime. The Transport is the egress
// fence: it injects biz.vc only toward registry-allowlisted hosts.
client := &http.Client{Transport: httpmw.NewTransport(&reg, http.DefaultTransport)}

func handle(ctx context.Context, wh ProviderWebhook) error {
    ctx, err := biz.WithValueContext(ctx, biz.ValueContext{
        Flow:       "payment.webhook",
        EntityID:   wh.PaymentIntentID,      // de-dup key across retries
        CustomerID: hash(wh.AccountID),      // pre-hashed — raw ids never enter biz.*
        Segment:    wh.Segment,
        Money:      biz.Money{Amount: wh.AmountMinor, Currency: "USD", Exponent: 2},
        Kind:       biz.KindFee,
    })
    if err != nil {
        return err
    }

    req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
        "https://payments.internal.example.com/internal/webhooks/process", body(wh))
    resp, callErr := client.Do(req)

    switch {
    case callErr != nil || resp.StatusCode >= 500:
        em.Record(ctx, "ingest", biz.ResultDeferred) // accepted, not yet processed
    case resp.StatusCode >= 400:
        em.Record(ctx, "ingest", biz.ResultFailed, emit.WithErr("rejected"))
    default:
        em.Record(ctx, "ingest", biz.ResultSuccess)
    }
    em.Flush(ctx) // Lambdas freeze between invocations — flush before returning
    return callErr
}
```

If the amount is not in the payload (only payments-service prices it),
set `Estimated: true` and leave `Money.Amount` zero with a valid
currency — the registry estimator's value fills in, and the report labels
that value as estimated, never merged with real amounts. For HTTP-shaped
Lambdas (function URLs, API Gateway proxy) the same stamping can live in
`httpmw.Middleware` with a `WithIngress` hook instead.

## 3. payments-service (processing stage)

The server middleware extracts the wire context so `biz.FromContext`
works everywhere downstream; you record the stage transition:

```go
mux := http.NewServeMux()
mux.HandleFunc("POST /internal/webhooks/process", func(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context() // ValueContext already extracted by the middleware
    if err := processWebhook(ctx); err != nil {
        em.Record(ctx, "process", biz.ResultFailed, emit.WithErr(classify(err)))
        http.Error(w, "processing failed", http.StatusInternalServerError)
        return
    }
    em.Record(ctx, "process", biz.ResultSuccess)
    w.WriteHeader(http.StatusNoContent)
})

handler := httpmw.Middleware(&reg)(mux)
```

If the Lambda hands off through a queue instead of HTTP, swap
`propagate/httpmw` for `propagate/sqs` (or `kafka`, `amqp`) — the flow
and stages do not change.

## 4. What each outage direction yields

**payments-service down, Lambdas up** — the well-instrumented case.
Every webhook is stamped at the door, so the report is deterministic:

| Leg | Grounded by |
|---|---|
| Realized loss | `ingest`/`process` failures, summed, de-duped by `EntityID` |
| Deferred value | webhooks recorded `deferred` that never reached `process`, by age; past `PT30M` they project to loss (`on_breach: lost`) |
| Customer impact | distinct hashed customers, segments, top accounts — from ingest stamps |

**Webhook Lambdas down** — the entry itself is dark, so no per-event
telemetry exists. That is what the **unrealized loss** leg is for: the
engine compares observed entry-stage volume against the seasonal
baseline (`hour_of_week` over `lookback_weeks`) and reports the value
that never happened — always a range, always labelled estimate. The
`recovery` curve then credits back the fraction providers redeliver.
`shortfall reconcile` against the provider ledger tells you how much
telemetry actually saw (coverage).

## 5. The report

```sh
shortfall impact \
  --registry registry.yaml \
  --from 2026-08-28T14:00:00Z --to 2026-08-28T15:30:00Z \
  --flow payment.webhook --format markdown
```

plus the query-adapter flags for your backend — for this CloudWatch
wiring, the `cwinsights` querier reads events from the EMF records and
metric legs from the CloudWatch metric store (see
[adapters.md](adapters.md) for which backend grounds which leg).

Next: [registry.md](registry.md) for every registry field,
[money.md](money.md) for what a "dollar" means here.
