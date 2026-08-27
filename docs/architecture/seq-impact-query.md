# Sequence — an impact question becomes a four-leg report

Query-time, not ingest-time: the engine asks any backend only the four
verbs, so nothing vendor-specific leaks past the adapter.

**Target design**: the frozen Querier/Report shapes are exact; the legs
and the `impact` CLI verb land with the engine milestones — until then
Compute refuses loudly rather than rendering zeros.

```mermaid
sequenceDiagram
    autonumber
    participant O as On-call / Slack bot
    participant CLI as shortfall impact
    participant E as engine.Compute
    participant R as registry
    participant Q as Querier adapter<br/>(promql / sql / …)
    participant B as Backend

    O->>CLI: --from … --to … --scope stage=capture
    CLI->>E: Request{window, scope, flows}
    E->>R: flows, SLAs, estimators,<br/>baseline + recovery config
    E->>Q: QueryMetric(sum biz_value_total{outcome=failed})
    Q->>B: translated (increase()/SQL/…)
    E->>Q: QueryEvents(group by entity — de-dup later successes)
    E->>Q: QueryMetric(biz_inflight_value by age_bucket, currency)
    E->>Q: QueryMetric(baseline lookback: N weeks of entries)
    E->>Q: QueryEvents(distinct customers; top-N by sum desc)
    Note over E,Q: a verb the backend cannot serve returns<br/>ErrUnsupported → the leg reports NotAvailable(reason)
    E-->>CLI: Report{realized, deferred, unrealized±,<br/>customers, coverage, severity, provenance}
    CLI-->>O: the ledger block —<br/>evidence tag on every leg,<br/>realized never summed with estimate
```
