# C4 Level 3 — capture and engine components

The paths money takes through the code. Left: one stage transition
becoming signals. Right: one question becoming a report.

```mermaid
flowchart LR
    subgraph capture["emit — one Record() call"]
        CTX["ctx: biz.vc member\n(decoded ValueContext)"]
        VAL["validate + PII guard\n(biz.CheckPII)"]
        DEDUP["de-dup LRU\n(flow, entity, stage)"]
        LABELS["label enforcement\nsegment ∈ registry enum\nflow/stage fallback: unregistered"]
        MP["MetricPoint(s)\nbiz_value_total · biz_txn_total\ndelta @ observation time"]
        OUT["biz.Outcome\nunsampled, trace-id linked"]
        DROP["biz_dropped_events_total{reason}\nnever a silent drop"]
    end
    CTX --> VAL --> DEDUP --> LABELS
    LABELS --> MP
    LABELS --> OUT
    VAL -->|invalid| DROP

    subgraph engine["engine — one Compute() call"]
        REQ["Request\nwindow · scope · flows"]
        REAL["realized\nΣ failed, de-duped by entity\ndeadline passed"]
        DEF["deferred\nin-flight by age bucket × currency\nSLA breaches → projected lost"]
        UNREAL["unrealized\nbaseline − actual, × recovery\nALWAYS a range"]
        CUST["customers\ndistinct · top-N by value\nor NotAvailable(reason)"]
        COV["coverage\ntelemetry Σ vs ledger"]
        REP["Report + provenance\nevidence tags per leg\nrealized NEVER summed with estimate"]
    end
    REQ --> REAL & DEF & UNREAL & CUST & COV --> REP
```

Evidence discipline, enforced by types: `Leg.Evidence` is
deterministic/estimate/trust; `EstLeg` has no single-number form — only
Low/Mid/High per currency; a querier without events yields
`NotAvailableReason`, never zeros.
