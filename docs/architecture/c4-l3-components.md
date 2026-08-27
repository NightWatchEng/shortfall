# C4 Level 3 — capture and engine components

The paths money takes through the code. **This is the TARGET design**:
the shapes are the v0.1.0 frozen contracts, the internals are the plan of
record for the emitter (M3) and engine legs (M6/M7) — the code refuses
loudly (`engine.ErrNotImplemented`) until each lands.

```mermaid
flowchart LR
    subgraph capture["emit — one Record() call (target design, lands M3)"]
        CTX["ctx: biz.vc member<br/>(decoded ValueContext)"]
        VAL["validate + PII guard<br/>(biz.CheckPII)"]
        DEDUP["in-process de-dup<br/>bounded, keyed (flow, entity, stage, result)<br/>so failed→success transitions always emit"]
        LABELS["label enforcement<br/>segment ∈ registry enum<br/>flow/stage fallback: unregistered"]
        MP["MetricPoint(s)<br/>biz_value_total · biz_txn_total<br/>delta @ observation time"]
        OUT["biz.Outcome<br/>unsampled, trace-id linked"]
        DROP["biz_dropped_events_total{reason}<br/>never a silent drop"]
    end
    CTX --> VAL --> DEDUP --> LABELS
    LABELS --> MP
    LABELS --> OUT
    VAL -->|rejected| DROP

    subgraph engine["engine — one Compute() call (frozen shapes; legs land M6/M7)"]
        REQ["Request<br/>window · scope · flows"]
        REAL["realized<br/>Σ failed, de-duped by entity<br/>(deterministic)"]
        DEF["deferred<br/>in-flight by age bucket × currency<br/>SLA breaches → projected lost"]
        UNREAL["unrealized<br/>baseline − actual, × recovery<br/>ALWAYS a range"]
        CUST["customers<br/>distinct · top-N by value<br/>or NotAvailable(reason)"]
        COV["coverage<br/>telemetry Σ vs ledger"]
        REP["Report + provenance<br/>evidence tags per leg<br/>realized NEVER summed with estimate"]
    end
    REQ --> REAL & DEF & UNREAL & CUST & COV --> REP
```

Cross-process rule the engine legs implement: replicas can each emit the
same outcome (the in-process de-dup cannot see across processes), so every
event-summing leg — realized AND coverage — de-duplicates by entity,
successes included.

Evidence discipline, enforced by the frozen types: `Leg.Evidence` is
deterministic/estimate/trust; `EstLeg` has no single-number form — only
Low/Mid/High per currency; a querier without events yields
`NotAvailableReason`, never zeros. Deadline-breach money belongs to the
DEFERRED leg's projected-lost, never to realized.
