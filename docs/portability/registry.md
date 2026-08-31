## 4. The flow registry

Governing ADRs: [ADR-0003](../adr/0003-value-context-propagation.md) (the
propagation allowlist), [ADR-0004](../adr/0004-metric-label-set.md) (the
segment enumeration), [ADR-0016](../adr/0016-value-stage-anchoring.md) (the
value stage). Field-by-field reference for humans:
[registry.md](../registry.md).

Every implementation in a deployment loads the *same* registry file and
MUST validate it *identically*. A file one implementation accepts and
another rejects is a deployment that reports different money depending on
which service was asked.

### 4.1 The smallest valid document

```yaml
version: 1
segments: [default]
flows:
  checkout:
    money: { kind: gmv }
    stages:
      - { name: pay, signals: ["http:POST /pay"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "stripe:charges" }
```

This is the `minimal` acceptance vector, and it is validated on every CI
run by the repository's doc-fence checker. Everything omitted here is
optional; everything present is required.

### 4.2 Validation rules that are easy to get wrong

**Unknown fields are errors.** A key the schema does not define MUST fail
the load. A typoed knob that silently defaults is a fence that is not
there, discovered during an incident.

**A second YAML document is an error.** Content after a `---` separator
MUST fail rather than be ignored, for the same reason.

**Durations are an ISO-8601 *subset*, not ISO-8601.** The accepted grammar
is `P[nD][T[nH][nM][nS]]`, strictly positive, at most ten years, with
units in descending order and days as exact 24-hour days. Months and years
are **rejected**: their length depends on when you ask, and an SLA that
means one thing in February and another in March is not an SLA. A port
that reaches for its standard library's ISO-8601 duration parser will
accept `P1M` and disagree with every other implementation. The `duration`
and `duration_reject` vectors pin the boundary, including the ten-year cap
and the rejection of Go-style `30m`.

**Two fences are declared here and enforced elsewhere.** The `segments`
enumeration is the metric-cardinality fence of 5.2; `propagation.allow_hosts`
is the egress fence of 3.5. Both MUST be validated at load — a malformed
allowlist pattern must fail *here*, not become a near-allow-all at match
time.

**The value stage is derived, not written.** A flow's value stage is its
declared `reconcile.stage` when present, and otherwise its **last**
declared stage. Every implementation must derive it the same way, because
it is where value is counted once per transaction; any cross-stage sum
multiply-counts. The acceptance vectors carry the derived `value_stage`
for exactly this reason.

**Severity thresholds are strictly decreasing.** Floors are minor units
per minute, ordered most-severe first, each strictly less than the one
before, so "the highest severity whose floor this rate clears" is
unambiguous. A duplicate label or an out-of-order floor is a config error,
never a silent tie.

**The estimator's exponent is declared.** It defaults to 2 and MUST be
declarable, so a JPY flow's estimate cannot inherit a USD estimator's
100× error.

### 4.3 Rejection classes

Every class below has at least one vector in
[`registry.json`](../../testkit/vectors/registry.json), each differing from
the reference document by exactly one edit — so a vector isolates the one
rule it is named for.

| Class | Rejected because |
|---|---|
| `yaml_syntax` | the document is not well-formed YAML |
| `unknown_field` | a key the schema does not define |
| `multi_document` | content after a `---` separator |
| `version_unsupported` | `version` is not 1 |
| `no_segments` | the segment enumeration is empty or absent |
| `segment_token` | a segment name outside `[a-z0-9._-]`, or longer than 32 |
| `segment_duplicate` | the same segment declared twice |
| `allow_host_pattern` | an allowlist entry that is not a bare lowercase DNS name or `*.domain` |
| `severity_sev_shape` | a severity label that is empty, over 32 characters, or contains whitespace |
| `severity_duplicate` | the same severity label twice |
| `severity_min_per_minute` | a floor that is not positive |
| `severity_order` | a floor not strictly below the previous one |
| `no_flows` | no flows declared |
| `flow_name_token` | a flow name outside `[a-z0-9._-]`, or longer than 64 |
| `money_kind` | `money.kind` outside `gmv`, `net_revenue`, `fee`, `take_rate` |
| `no_stages` | a flow with no stages |
| `stage_token` | a stage name outside `[a-z0-9._-]`, or longer than 32 |
| `stage_duplicate` | the same stage name twice in one flow |
| `stage_no_signals` | a stage declaring no signals |
| `stage_empty_signal` | a signal that is empty or whitespace |
| `currency_code` | a declared currency that is not an ISO 4217 alphabetic code |
| `currency_duplicate` | the same currency twice |
| `sla_unknown_stage` | an SLA naming a stage the flow does not declare |
| `sla_deadline` | a deadline outside the duration subset |
| `sla_on_breach` | `on_breach` other than `lost` or `at_risk` |
| `estimator_default_minor` | a default that is not positive minor units |
| `estimator_segment_unknown` | `by_segment` naming a segment outside the enumeration |
| `estimator_by_segment_value` | a per-segment amount that is not positive |
| `estimator_exponent_range` | an estimator exponent outside `[0, 4]` |
| `baseline_seasonality` | a seasonality model other than `hour_of_week` |
| `baseline_lookback` | `lookback_weeks` below 1 |
| `recovery_model` | a recovery model other than `usage_loss_curve` |
| `recovery_fraction` | `recovered_fraction` outside `[0, 1]`, or not a finite number |
| `recovery_within_missing` | a positive recovered fraction with no window |
| `recovery_within_without_fraction` | a window with a zero recovered fraction |
| `reconcile_source_required` | a flow with no reconcile source |
| `reconcile_source_scheme` | a reconcile source outside the known schemes (`sql:`, `stripe:`) |
| `reconcile_stage_unknown` | `reconcile.stage` naming a stage the flow does not declare |

**Finiteness is checked before the bound.** A `[0, 1]` bound written as
the pair `< 0 || > 1` admits NaN, which fails both halves — and NaN then
fails the subsequent `> 0` test too, so such a flow is never even asked
for its `within` window. An implementation MUST therefore reject a
non-finite `recovered_fraction` explicitly, under `recovery_fraction`,
rather than rely on the range comparisons to do it. The
`recovery_fraction_nan` vector pins this.

### 4.4 Allowlist matching

An entry is either an exact lowercase DNS name or a `*.domain` pattern.

- `*.domain` matches any host one or more labels deeper than `domain`, and
  **never `domain` itself**.
- An exact entry grants nothing below it: `api.example.com` does not
  admit `x.api.example.com`.
- The label boundary is part of the match, so `evil-internal.example.com`
  does not match `*.internal.example.com`.
- The input is a **bare hostname**: no port, no trailing dot. Case is
  normalized; anything else malformed is **denied, not cleaned** —
  cleaning is how `evil.com.` gets past an allowlist.
- An empty or absent allowlist denies everything.

Twelve `host_allowlist` vectors pin these, including both verdicts.

---
