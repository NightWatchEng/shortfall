# Registry reference

The registry is the versioned, Finance-co-signed YAML that answers, up front,
the questions otherwise relitigated during every incident: what counts as
money, where a flow's stages live, when deferred value becomes lost, what an
unknown amount is worth, and how much demand returns after recovery. It also
declares two fences other layers enforce — the segment enumeration (metric
cardinality, ADR-0004) and the propagation host allowlist (ADR-0003).

Validation is loud and names the offending field. Unknown keys are errors (a
typo must fail, not silently default). Load with `registry.Load(path)` or
validate with `shortfall validate <registry.yaml>`.

## Top level

```yaml
version: 1                      # required; only 1 is supported
segments: [smb, enterprise]     # required, ≥1; the metric-cardinality fence.
                                # lowercase [a-z0-9._-], ≤32 chars, unique
propagation:
  allow_hosts:                  # where biz context may egress (deny by default)
    - "*.internal.example.com"  # *.domain matches deeper subdomains only
    - "api.example.com"         # or an exact bare hostname
severity:                       # optional; the $/min-at-risk ladder (ADR-0013)
  - { sev: SEV1, min_per_minute: 100000 }   # most-severe first,
  - { sev: SEV2, min_per_minute: 10000 }    # STRICTLY DECREASING floors,
  - { sev: SEV3, min_per_minute: 1000 }     # minor units/min, unique names
flows:
  invoice.pay: { ... }          # required, ≥1; keys are lowercase flow names
```

## A flow

```yaml
invoice.pay:
  money: { kind: fee }          # gmv | net_revenue | fee | take_rate — see money.md
  currencies: [USD]             # optional; declared currencies bound cardinality.
                                # EMPTY MEANS UNDECLARED (any accepted), not "none"
  stages:                       # ordered; stages[0] is the ENTRY stage
    - { name: auth,    signals: ["http:POST /pay", "provider:stripe.payment_intent"] }
    - { name: capture, signals: ["queue:capture.q", "webhook:payment_intent.succeeded"] }
    - { name: settle,  signals: ["queue:settle.q"] }
  sla:                          # per-stage deadline + what a breach means
    capture: { deadline: PT30M, on_breach: lost }     # lost -> projected loss
    settle:  { deadline: P1D,   on_breach: at_risk }  # at_risk -> a breach, not loss
  estimator:                    # value for amounts ingress does not know
    default_minor: 18750        # minor units at `exponent` (default 2)
    exponent: 2
    by_segment: { smb: 14200, enterprise: 91000 }
  baseline:                     # the counterfactual expectation (ADR-0006)
    seasonality: hour_of_week   # the only v0 model
    lookback_weeks: 8           # ≥1; needs this much querier metric history
    holidays: us                # optional calendar name (v0 does not yet apply it)
  recovery:                     # usage-loss curve for suppressed demand
    model: usage_loss_curve
    recovered_fraction: 0.6     # 0..1; fraction of suppressed demand that returns
    within: PT2H                # required when recovered_fraction is set
  reconcile:                    # the ledger source coverage is measured against
    source: "sql:ledger.payments"   # scheme:path — known schemes: sql:, stripe:
```

## Field notes

- **Durations** are ISO-8601 (`PT30M`, `PT90S`, `P1D`, `P1DT12H`). Go syntax
  (`30m`) is rejected; months/years (`P1M`, `P1Y`) are ambiguous and rejected;
  a zero duration is a config error.
- **`money.kind`** picks the money definition the flow reports under; the emit
  layer refuses to record an outcome whose kind is invalid. See [money.md](money.md).
- **`stages[0]` is the entry stage** — the counterfactual leg counts stage
  entries there (`biz_txn_total` at the entry stage) to measure suppressed
  demand.
- **`sla[stage].on_breach`**: `lost` means value past the deadline is projected
  loss; `at_risk` means it is a breach (counted in `SLABreaches`) but not loss.
  Breach detection is at ADR-0005 age-bucket granularity — a deadline beyond
  the top bucket floor (2h) never registers, so a long SLA (P1D) is a
  conservative lower bound.
- **`estimator.exponent`** must match the flow's currency minor unit (a JPY
  flow wants `exponent: 0`); an estimate is never applied under a mismatched
  exponent.
- **`severity`** floors are minor units per minute at your currency/exponent;
  the suggestion is the most-severe level the realized+deferred $/min clears,
  evaluated per currency (ADR-0013). Omit the section for no suggestion.

The reference registry (`registry/testdata/registry.yaml`) is a complete,
valid example — copy it and adjust.
