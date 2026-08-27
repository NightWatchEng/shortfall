# ADR-0004 — Metrics carry exactly six labels; cardinality is a library guarantee

Status: accepted (ratified at the M2 interface freeze, 2026-08-27)
Date: 2026-08-27

## Context

Every vendor postmortem about "business metrics" ends in a cardinality
bill. Amounts and entity ids are unbounded; time-series databases are not.
The proposal's rule: amounts and ids ride on events only, metrics carry
sums and counts with bounded labels. A rule that users must remember is a
rule that fails; this one must be enforced by the emitting code itself.

## Decision

The metric families and their **exact, per-family label sets** are fixed —
no family ever gains a label, and no family exists beyond these five:

| family | labels |
|---|---|
| `biz_value_total` | `flow`, `stage`, `outcome`, `currency`, `kind`, `segment` |
| `biz_txn_total` | `flow`, `stage`, `outcome`, `currency`, `segment` |
| `biz_inflight_value` | `flow`, `stage`, `age_bucket`, `currency` |
| `biz_provider_calls_total` | `provider`, `op`, `outcome` |
| `biz_dropped_events_total` | `reason` (fixed enum, see ADR-0002) |

- `flow`, `stage`, `segment` values must come from the registry, with a
  defined fallback for each so money is never silently lost:
  - `segment` outside the enumeration → emits with `segment=""`, logged
    warning.
  - `flow` or `stage` not in the registry → emits with the fixed value
    `unregistered` (sums stay complete, cardinality stays bounded, the
    misconfiguration is visible on a dashboard); the outcome **event**
    keeps the raw names for diagnosis.
- `provider` and `op` on `biz_provider_calls_total` are bounded by
  construction (a handful of payment providers, each with a small API
  surface); they are adapter-supplied constants, never request data.
- `biz.entity.id` and `biz.customer.id` are events-only attributes; any
  exporter or code path that would place them on a metric is a defect
  (review charter item 4), and the emit layer's label construction makes it
  structurally impossible rather than discouraged.

## Consequences

- Worst-case series count is computable at registry-review time from: the
  registry enumerations (`flow`, `stage`, `segment`, `kind`), the fixed
  library enums (`outcome`, `age_bucket`, `reason`), and `currency` — the
  one data-driven axis, capped by ISO 4217 (~180) and boundable per flow by
  declaring expected currencies in the registry.
- Per-customer questions are answered from the event sink, never from
  metrics; backends without an event sink honestly report
  `NotAvailable` for the customers leg.
- Adding a label is an interface change with an ADR, not a patch.
