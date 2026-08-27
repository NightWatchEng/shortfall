# ADR-0005 — Fixed in-flight age buckets: lt1m, 1m-5m, 5m-30m, 30m-2h, gt2h

Status: accepted (ratified at the M2 interface freeze, 2026-08-27)
Date: 2026-08-27

## Context

Deferred value is the leg most tools cannot express: money in queues,
retries, and DLQs is not lost — until an SLA turns it into loss. Pagers and
dashboards need a small, stable set of age buckets; per-registry
configurable buckets would fragment dashboards across flows and make the
`age_bucket` label's cardinality registry-dependent, violating the spirit
of ADR-0004.

## Decision

Five fixed buckets, identical for every flow and every deployment:

| bucket | meaning |
|---|---|
| `lt1m` | normal in-flight churn |
| `1m-5m` | worth a glance |
| `5m-30m` | inside most capture SLAs, aging |
| `30m-2h` | typical SLA breach territory |
| `gt2h` | almost certainly converting to loss |

- Bucket boundaries align with typical payment-SLA breakpoints — e.g. a
  30-minute capture SLA, the shape the M2 reference registry will use —
  and are chosen for pager legibility, not statistical elegance.
- SLA evaluation itself uses the **exact** per-message deadline from the
  registry — buckets are for the gauge and the pager, never for breach
  math.

## Consequences

- `biz_inflight_value{flow,stage,age_bucket,currency}` has a fixed,
  documented worst-case cardinality.
- Cross-flow dashboards and alert rules are copy-pasteable.
- If a future flow genuinely needs different buckets, that is an ADR
  revision changing them for everyone — the cost is deliberate.
