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
| `biz.entity.id` | string | invoice/order id — the de-dup key. May be reused across attempts; not assumed unique per transaction. |
| `biz.customer.id` | string | opaque, already-hashed customer handle (`h:...`); never raw PII. |
| `biz.segment` | string | bounded segment enumeration (e.g. `smb`, `enterprise`). Absent, not empty, when the flow declares none. |
| `biz.amount.minor` | int | amount in ISO-4217 **minor units** (integer; never float). |
| `biz.amount.currency` | string | ISO-4217 alphabetic code. |
| `biz.amount.exponent` | int | minor-unit decimal places (0–4), so amount is unambiguous. |
| `biz.value.kind` | string | money definition: `gmv` \| `net_revenue` \| `fee` \| `take_rate`. |
| `biz.amount.estimated` | boolean | true when the amount came from the registry estimator, not observed. |
| `biz.sla.deadline` | string | RFC-3339 UTC deadline, when the ValueContext carries one. Absent otherwise. |

**Amended 2026-08-30 (workspace-cnz).** This table previously gave
`biz.entity_id`, `biz.customer_id`, `biz.money.amount_minor`,
`biz.money.currency`, `biz.money.exponent`, `biz.kind` and `biz.estimated`
— a third spelling of the same facts, agreeing with neither ADR-0002's
canonical JSON nor any shipped exporter. The names above are the ones on
the wire, defined once as the `biz.Attr*` constants and pinned by
`testkit/vectors/outcome-event.json`. See ADR-0002 for the settlement.

**Amended 2026-08-30 (workspace-8yq).** The four money attributes moved
into a consistent dotted namespace: `biz.amount_minor` → `biz.amount.minor`,
`biz.currency` → `biz.amount.currency`, `biz.exponent` →
`biz.amount.exponent`, `biz.amount.est` → `biz.amount.estimated`. The
settlement above deliberately froze the shipped spelling verbatim; this
pre-1.0 rename (a founder decision, wire-format break) made the rule
inferable before v1.0 froze it for good.

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
`abandoned`, `unknown` — the bounded result enumeration — on the transaction
families. On `biz_provider_calls_total` it is the narrower provider-health
pair `success`, `failed`: the provider either returned a definitive answer or
it did not, and a declined card is the provider answering correctly.

`provider` and `op` are adapter-supplied constants. A producer MUST bound
them rather than trust that: `emit` admits a fixed number of distinct
(`provider`, `op`) pairs per emitter and collapses the rest to `other`, so a
caller that passes request data inflates one series instead of the family.

## Design invariants a conforming producer MUST keep

- Amounts are integer minor units with an explicit exponent; **no floats**, and
  **no summing across currencies**.
- Estimated amounts ride the event (`biz.amount.estimated=true`) and are **excluded**
  from `biz_value_total` — realized and estimated are never merged.
- Metric label sets are fixed and bounded; ids and amounts never become labels.
- Every event that carries value carries the co-emitted count (value and count
  gauges/counters move together), so the two never disagree.

These are the shapes the reference implementation, its conformance suite, and
the ADRs already enforce; the proposal is to make them a shared convention.
