# Draft: OpenTelemetry semantic conventions for `biz.*` attributes

**Status: draft, not submitted.** This records the attribute and metric shapes
shortfall emits so they are stable and reviewable now. Upstream submission to
OpenTelemetry happens only after internal adoption evidence — deliberately not
filed yet. The `biz.*` prefix is library-neutral on purpose: the convention
survives a rename or fork of shortfall itself.

## Rationale

Observability conventions describe *system* health (latency, errors,
saturation). None describe *business* outcomes attached to that telemetry — the
flow, the money, the customer — which is what turns "the API had a bad hour"
into "the incident cost $X and hit these accounts." These conventions add that
layer without inventing a transport: they ride existing spans, logs, and
metrics.

## Attributes (on spans / log records / baggage)

The unit is the **value context** attached to a unit of business work and
propagated across service boundaries (deny-by-default egress; see ADR-0003).

| Attribute | Type | Notes |
|---|---|---|
| `biz.flow` | string | the business flow, e.g. `invoice.pay`. Bounded (registry-declared). |
| `biz.entity_id` | string | invoice/order id — the de-dup key. May be reused across attempts; not assumed unique per transaction. |
| `biz.customer_id` | string | opaque, already-hashed customer handle (`h:...`); never raw PII. |
| `biz.segment` | string | bounded segment enumeration (e.g. `smb`, `enterprise`). |
| `biz.money.amount_minor` | int | amount in ISO-4217 **minor units** (integer; never float). |
| `biz.money.currency` | string | ISO-4217 alphabetic code. |
| `biz.money.exponent` | int | minor-unit decimal places (0–4), so amount is unambiguous. |
| `biz.kind` | string | money definition: `gmv` \| `net_revenue` \| `fee` \| `take_rate`. |
| `biz.estimated` | boolean | true when the amount came from the registry estimator, not observed. |

Bounded attributes (`biz.flow`, `biz.segment`, and the metric labels below) are
the metric-cardinality fence: unbounded values (ids, amounts) ride events/spans,
never metric labels (ADR-0004).

## Metrics

Fixed families with fixed, bounded label sets (ADR-0004/0005/0012):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `biz_value_total` | counter | `flow,stage,outcome,currency,kind,segment` | realized value sum (minor units); estimates excluded |
| `biz_txn_total` | counter | `flow,stage,outcome,currency,segment` | transaction count |
| `biz_inflight_value` | gauge | `flow,stage,age_bucket,currency` | in-flight (deferred) value by age bucket |
| `biz_inflight_count` | gauge | `flow,stage,age_bucket,currency` | in-flight transaction count by age bucket |
| `biz_provider_calls_total` | counter | `provider,op,outcome` | downstream provider calls (provider-health) |
| `biz_dropped_events_total` | counter | `reason` | telemetry the emitter rejected (loud, never silent) |

`age_bucket` is the fixed 5-value set of ADR-0005 (`lt1m`, `1m-5m`, `5m-30m`,
`30m-2h`, `gt2h`). `outcome` is one of `success`, `failed`, `deferred`,
`abandoned`, `unknown` — the bounded result enumeration.

## Design invariants a conforming producer MUST keep

- Amounts are integer minor units with an explicit exponent; **no floats**, and
  **no summing across currencies**.
- Estimated amounts ride the event (`biz.estimated=true`) and are **excluded**
  from `biz_value_total` — realized and estimated are never merged.
- Metric label sets are fixed and bounded; ids and amounts never become labels.
- Every event that carries value carries the co-emitted count (value and count
  gauges/counters move together), so the two never disagree.

These are the shapes the reference implementation, its conformance suite, and
the ADRs already enforce; the proposal is to make them a shared convention.
