# Sequence — Stripe events become outcomes

Same signals, different capture point: metadata stamped at creation makes
every webhook arrive pre-attributed; the wrapped client sees the failures
webhooks never report.

```mermaid
sequenceDiagram
    autonumber
    participant S as Your service
    participant WC as Wrapped Stripe client
    participant ST as Stripe
    participant WH as Webhook receiver
    participant X as Exporter → backend

    S->>WC: create PaymentIntent<br/>WithStripeMetadata(params, vc)
    WC->>ST: API call (metadata: flow, entity, customer hash)
    ST-->>WC: response | timeout | 5xx
    WC->>X: Record("auth", …) from the SYNC response<br/>+ provider-call counter
    Note over WC: timeouts and 5xx are auth truth<br/>webhooks will never deliver

    ST--)WH: payment_intent.succeeded / failed<br/>charge.dispute.created …
    Note over WH: signature verified;<br/>delivery may be HOURS late during outages
    WH->>X: Record(stage, result,<br/>WithAt(event.created), WithSource("stripe:webhook"))
    Note over X: provider event time, not receipt time —<br/>late delivery must not move money across windows

    Note over ST,WH: Stripe down ⇒ zero webhooks — that silence IS<br/>the counterfactual leg's signal, by design
```
