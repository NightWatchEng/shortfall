# C4 Level 2 — containers

Four layers; only the top one knows your vendors. The core emits two
normalized signals and never touches a backend; adapters translate on the
way out and back in.

```mermaid
flowchart TB
    subgraph app["Instrumented service (your code)"]
        MW["propagate/httpmw\nBaggage extract/inject + ingress stamping\n+ egress allowlist (ADR-0003)"]
        CARRIERS["propagate/kafka|sqs|amqp\nCarrier interfaces, zero client deps"]
    end

    subgraph core["shortfall core module (zero heavy deps)"]
        BIZ["biz\nMoney (int64 minor) · ValueContext · Outcome\nPII guard · biz.vc codec"]
        REG["registry\nflows · stages · SLAs · estimators\nsegments fence · allowlist fence"]
        EMIT["emit\nRecord → MetricPoint + Outcome\nde-dup LRU · label enforcement\nin-flight tracker"]
        QUERY["query\nthe 4-verb AST: sum/count/group-by/range\n+ event order & distinct-count"]
        ENGINE["engine\nrealized · deferred · unrealized\ncustomers · coverage · severity"]
        CLI["cmd/shortfall\nvalidate · impact · reconcile · simulate"]
    end

    subgraph adapters["adapters/* (each a nested Go module)"]
        EXP["export/*\notlp (default) · prometheus · statsd\ncloudwatch EMF · splunkhec · datadog · loki"]
        QAD["query/*\npromql · sql · logql · cwinsights · spl"]
        PAY["payment/stripe\nwebhook receiver · wrapped client · reconciler"]
        INC["incident/*\nslack /impact · incident.io · pagerduty ..."]
    end

    subgraph verify["ground truth (never ships)"]
        HARNESS["examples/checkout\nseeded simulation + omniscient ledger"]
        TESTKIT["testkit\nexpected values · golden fixtures\nexporter conformance suite"]
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
