# Sequence — record and propagate across the queue

The direct fix for "correlation_id sometimes isn't there": value context
is a first-class propagated thing, one header, re-attached by the
consumer wrapper with no per-team instrumentation ask.

```mermaid
sequenceDiagram
    autonumber
    participant U as Client
    participant API as api service<br/>(httpmw server middleware)
    participant K as capture.q<br/>(Kafka/SQS/AMQP)
    participant W as capture-worker<br/>(carrier + httpmw hook)
    participant X as Exporter → backend

    U->>API: POST /pay
    Note over API: ingress stamping: first hop that recognizes<br/>the flow builds ValueContext<br/>(flow, entity, hashed customer, amount)
    API->>API: ctx, err = WithValueContext(ctx, vc)<br/>(encode failures reject loudly: oversize, PII)
    API->>X: Record("auth", success)<br/>MetricPoints + Outcome (unsampled)
    API->>K: publish msg<br/>carrier copies ONE header: biz.vc
    Note over K: minutes pass; trace context may be<br/>re-rooted or dropped — biz.vc survives
    K->>W: consume msg
    W->>W: vc, ok, err = decode biz.vc header
    Note over W: err ≠ absent: a corrupted header is<br/>counted loudly, never mistaken for missing
    W->>X: Record("capture", failed, WithErr(...))
    Note over X: the failing hop already carries flow,<br/>entity, amount — attribution needs no lookup
```

Egress rule (ADR-0003): the client RoundTripper injects `biz.vc` only
toward registry-allowlisted hosts — amounts and customer hashes never
ride to third parties by default.
