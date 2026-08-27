# ADR-0004 — Metrics carry exactly six labels; cardinality is a library guarantee

Status: proposed (ratify at the M2 interface freeze)
Date: 2026-08-27

## Context

Every vendor postmortem about "business metrics" ends in a cardinality
bill. Amounts and entity ids are unbounded; time-series databases are not.
The proposal's rule: amounts and ids ride on events only, metrics carry
sums and counts with bounded labels. A rule that users must remember is a
rule that fails; this one must be enforced by the emitting code itself.

## Decision

- The metric label set is **exactly** `flow`, `stage`, `outcome`,
  `currency`, `kind`, `segment` — plus `age_bucket` on
  `biz_inflight_value` only. Nothing else, ever, on any exporter.
- `flow`, `stage`, `segment` values must come from the registry.
  A `segment` value outside the registry's enumeration is **dropped with a
  logged warning** — the metric emits with `segment=""` rather than
  minting a new series.
- `biz.entity.id` and `biz.customer.id` are events-only attributes; any
  exporter or code path that would place them on a metric is a defect
  (review charter item 4), and the emit layer's label construction makes it
  structurally impossible rather than discouraged.
- Metric families are fixed: `biz_value_total`, `biz_txn_total`,
  `biz_inflight_value`, `biz_provider_calls_total`
  (`provider`,`op`,`outcome` labels on the last, same bounded discipline).

## Consequences

- Worst-case series count is the product of registry enumerations — known
  at registry-review time, before anything ships.
- Per-customer questions are answered from the event sink, never from
  metrics; backends without an event sink honestly report
  `NotAvailable` for the customers leg.
- Adding a label is an interface change with an ADR, not a patch.
