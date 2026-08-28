# ADR-0012 — biz_inflight_count: a companion count gauge for exact deferred breach/txn counts

Status: accepted (ADR-0004 metric-family amendment, 2026-08-28)
Date: 2026-08-28

## Context

The deferred leg reports in-flight VALUE by age bucket from
`biz_inflight_value{flow,stage,age_bucket,currency}` (ADR-0004, ADR-0005). That
gauge carries value only. So `DeferredLeg.Count` (in-flight transaction count)
and `DeferredLeg.SLABreaches` (breaching transaction count) could not be filled
exactly — they were left 0 with a caveat, and breach was expressed only as
`ProjectedLostMinor` (breached VALUE). The founder chose the value-gauge source
with honest gaps over an events-based count; filling the exact counts needs a
companion count gauge, which is a frozen metric-family amendment (ADR-0004) and
so goes through this reviewed ADR (workspace-lte).

## Decision

Add one gauge to the in-flight family:

`biz_inflight_count{flow,stage,age_bucket,currency}` — the number of in-flight
transactions in each (flow, stage, age bucket, currency), emitted by the
`InFlightTracker` on the same publish cycle as `biz_inflight_value`.

- Its label set is IDENTICAL to `biz_inflight_value` (ADR-0004's "fixed
  per-family label set" — the two gauges of the in-flight family share one
  shape), so its worst-case cardinality is the same bounded product and
  cross-flow dashboards stay copy-pasteable. `currency` is retained for family
  parity; a count is currency-agnostic, so the deferred leg sums it across
  currencies for a total.
- The tracker already snapshots the in-flight set per (combo, bucket); it now
  emits the bucket's count alongside its value. Value and count are published
  together from the SAME snapshot, so they never disagree.
- `emit.Emitter.SetInFlight` gains a `count int64` parameter and emits BOTH
  gauges. This is a small interface amendment (the only implementation is
  `Std`); a negative count is rejected like invalid money.
- The deferred leg reads `biz_inflight_count` to fill `Count` (sum over buckets
  and currencies) and `SLABreaches` (sum over breaching buckets — the same
  breach test used for `ProjectedLostMinor`). The count-unavailable caveat is
  dropped when the gauge is present; a value-only source (no count gauge) still
  degrades honestly, keeping the caveat.

## Consequences

- `DeferredLeg.Count` (sum over all buckets) is now exact against the tracker's
  snapshot. `SLABreaches` uses the same bucket-FLOOR breach test as
  ProjectedLostMinor, so it is a conservative LOWER BOUND: a deadline that falls
  inside a bucket, or beyond the largest bucket floor (gt2h = 120m — so a P1D
  SLA never registers as breached via buckets), is not attributed. This is the
  ADR-0005 tradeoff (buckets are for the pager; exact breach math needs the
  per-message deadline). bead 7.3's superseded AC ("breach count agrees with
  ground truth") is back in force to the bucket granularity the gauge supports.
- The in-flight metric family is two gauges, not one; exporters and dashboards
  that hard-coded one gauge name see the second additively (no relabeling).
- `SetInFlight`'s signature changed; the in-tree caller (the tracker) and tests
  are updated. External emitters implementing the interface add the count they
  already track.
