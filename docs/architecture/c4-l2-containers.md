# C4 Level 2 — containers

Four layers; only the top one knows your vendors. The core emits two
normalized signals and never touches a backend; adapters translate on the
way out and back in.

**Reading this diagram honestly:** solid boxes in `core` exist today
(the v0.1.0 frozen surface — `emit`, `query`, `engine` are frozen
contracts whose implementations land in M3/M6). Everything in
`adapters/*` is a **planned family** (see `adapters/README.md`) landing
per milestone, and the CLI serves `validate` today with the other verbs
arriving with the engine.

```mermaid
flowchart TB
    subgraph app["Instrumented service (your code)"]
        MW["propagate/httpmw<br/>Baggage extract/inject + ingress stamping<br/>+ egress allowlist (ADR-0003) — lands M3"]
        CARRIERS["propagate/kafka | sqs | amqp<br/>Carrier interfaces, zero client deps — lands M3"]
    end

    subgraph core["shortfall core module (zero heavy deps)"]
        BIZ["biz<br/>Money (int64 minor) · ValueContext · Outcome<br/>PII guard · biz.vc codec"]
        REG["registry<br/>flows · stages · SLAs · estimators<br/>segments fence · allowlist fence"]
        EMIT["emit (frozen contract; impl lands M3)<br/>Record → MetricPoint + Outcome<br/>in-flight tracker"]
        QUERY["query (frozen contract)<br/>the 4-verb AST: sum/count/group-by/range<br/>+ event order & distinct-count"]
        ENGINE["engine (frozen contract; legs land M6/M7)<br/>realized · deferred · unrealized<br/>customers · coverage · severity"]
        CLI["cmd/shortfall<br/>validate (today)<br/>impact · reconcile · simulate (with the engine)"]
    end

    subgraph adapters["adapters/* — PLANNED families, each a nested Go module"]
        EXP["export/*<br/>otlp (default) · prometheus · statsd<br/>cloudwatch EMF · splunkhec · datadog · loki"]
        QAD["query/*<br/>promql · sql · logql · cwinsights · spl"]
        PAY["payment/stripe<br/>webhook receiver · wrapped client · reconciler"]
        INC["incident/*<br/>slack /impact · incident.io · pagerduty …"]
    end

    subgraph verify["ground truth (exists today; never ships)"]
        HARNESS["examples/checkout<br/>seeded simulation + omniscient ledger"]
        TESTKIT["testkit<br/>expected values · golden fixtures<br/>exporter conformance suite (lands M4)"]
    end

    MW -->|ValueContext in ctx| EMIT
    CARRIERS -->|one header: biz.vc| MW
    BIZ --- EMIT
    REG --- EMIT
    EMIT -->|Exporter interface| EXP
    ENGINE -->|Querier interface| QAD
    REG --- ENGINE
    QUERY --- ENGINE
    PAY -->|Outcomes| EMIT
    ENGINE --> CLI
    CLI --> INC
    HARNESS --> TESTKIT
    TESTKIT -.->|goldens judge| ENGINE
```

Boundary rules the gate enforces mechanically:

- `engine` imports only `query`, `registry`, `biz`, and its own
  subpackages (`engine-import-boundary` rule — an allowlist, not a
  blocklist).
- Amounts and ids ride events only; metrics carry the fixed ADR-0004
  label families.
- A Prometheus user never pulls stripe-go: every adapter is its own
  nested module.
