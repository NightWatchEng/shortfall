# C4 Level 2 — containers

Four layers; only the top one knows your vendors. The core emits two
normalized signals and never touches a backend; adapters translate on the
way out and back in.

```mermaid
flowchart TB
    subgraph app["Instrumented service (your code)"]
        MW["propagate/httpmw<br/>Baggage extract / inject ·<br/>ingress stamping · egress allowlist"]
        CARRIERS["propagate/kafka · sqs · amqp<br/>carrier interfaces, zero client deps"]
    end

    subgraph core["shortfall core module (zero heavy deps)"]
        BIZ["biz<br/>Money (int64 minor) · ValueContext ·<br/>Outcome · PII guard · biz.vc codec"]
        REG["registry<br/>flows · stages · SLAs · estimators ·<br/>segments fence · allowlist fence"]
        EMIT["emit<br/>Record → MetricPoint + Outcome ·<br/>in-flight tracker · loud drops"]
        QUERY["query<br/>the 4-verb AST — sum · count ·<br/>group-by · range + event order"]
        ENGINE["engine<br/>realized · deferred · unrealized ·<br/>customers · coverage · severity"]
        CLI["cmd/shortfall<br/>validate · impact · reconcile"]
    end

    subgraph adapters["adapters/* — each a nested Go module"]
        EXP["export/*<br/>prometheus · cloudwatch EMF"]
        QAD["query/*<br/>promql · cwinsights · sql"]
        PAY["payment/stripe<br/>webhook receiver ·<br/>wrapped client · reconciler"]
        INC["incident/slack<br/>post + refresh the impact ledger"]
    end

    subgraph verify["ground truth (never ships)"]
        HARNESS["examples/checkout<br/>seeded simulation + omniscient ledger"]
        TESTKIT["testkit<br/>expected values · golden fixtures ·<br/>exporter conformance suite"]
    end

    MW -->|"ValueContext in ctx"| EMIT
    CARRIERS -->|"one header — biz.vc"| MW
    BIZ --- EMIT
    REG --- EMIT
    EMIT -->|"Exporter interface"| EXP
    ENGINE -->|"Querier interface"| QAD
    REG --- ENGINE
    QUERY --- ENGINE
    PAY -->|"Outcomes"| EMIT
    ENGINE --> CLI
    CLI --> INC
    HARNESS --> TESTKIT
    TESTKIT -.->|"goldens judge"| ENGINE
```

Boundary rules the gate enforces mechanically:

- `engine` imports only `query`, `registry`, `biz`, and its own
  subpackages (`engine-import-boundary` rule — an allowlist, not a
  blocklist).
- Amounts and ids ride events only; metrics carry the fixed ADR-0004
  label families.
- A Prometheus user never pulls stripe-go: every adapter is its own
  nested module.
