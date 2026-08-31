## 5. Metrics and the outcome event

Governing ADRs: [ADR-0004](../adr/0004-metric-label-set.md) (label sets),
[ADR-0002](../adr/0002-outcome-event-transport.md) (event transport and
shape), [ADR-0005](../adr/0005-inflight-age-buckets.md) (age buckets),
[ADR-0012](../adr/0012-inflight-count-gauge.md) (the in-flight count gauge),
[ADR-0018](../adr/0018-provider-call-writer.md) (the provider-call writer and
its cardinality fence).
Draft convention write-up: [internal/semconv.md](../internal/semconv.md)
(maintainer-facing, not published to the wiki).

### 5.1 What is frozen and what is not

`semconv.md` is marked **draft** as an OpenTelemetry proposal — meaning it
has not been submitted upstream, deliberately, until there is adoption
evidence. That draft status is about *submission*, and a port should not
read it as "these shapes are unsettled". They are not equally settled,
though, so:

| Shape | Status for a port |
|---|---|
| the set of metric families | **frozen** — a new family is an ADR, not a patch |
| each family's label set | **frozen** — a family never gains a label |
| the `outcome` value enumeration | **frozen** |
| the `age_bucket` value enumeration | **frozen** |
| the `reason` value enumeration on drops | **frozen** |
| the label fallbacks (5.3) | **frozen** |
| the *facts* an outcome event carries | **frozen** |
| the *serialized key names* of an outcome event | **frozen** — see 7.1 |
| the OTel attribute names in `semconv.md` | draft, pending upstream submission |

### 5.2 Metric families

Six families exist. No implementation may emit a seventh under the `biz_`
prefix, and no family may gain a label.

| Family | Type | Labels |
|---|---|---|
| `biz_value_total` | counter | `flow`, `stage`, `outcome`, `currency`, `kind`, `segment` |
| `biz_txn_total` | counter | `flow`, `stage`, `outcome`, `currency`, `segment` |
| `biz_inflight_value` | gauge | `flow`, `stage`, `age_bucket`, `currency` |
| `biz_inflight_count` | gauge | `flow`, `stage`, `age_bucket`, `currency` |
| `biz_provider_calls_total` | counter | `provider`, `op`, `outcome` |
| `biz_dropped_events_total` | counter | `reason` |

> ADR-0004's own table lists five families and says no family exists
> beyond them; `biz_inflight_count` was added afterwards by ADR-0012, as
> an amendment. Six is the current contract. Reconciling ADR-0004's
> wording with ADR-0012 is tracked separately — see 7.2.

Bounded value enumerations:

- `outcome` ∈ `success`, `failed`, `deferred`, `abandoned`, `unknown` on the
  transaction families; on `biz_provider_calls_total` it is the narrower
  provider-health pair `success`, `failed`
- `age_bucket` ∈ `lt1m`, `1m-5m`, `5m-30m`, `30m-2h`, `gt2h`
- `reason` ∈ `invalid`, `overflow`, `export`

`currency` is the one data-driven label axis, bounded in practice by ISO
4217 and boundable per flow by declaring `currencies` in the registry.
`provider` and `op` are adapter-supplied constants, never request data — and
a conforming producer MUST enforce that rather than assume it. The reference
implementation admits a fixed number of distinct (`provider`, `op`) pairs per
emitter (64) and collapses every pair past it to the fixed value `other`; the
call is still counted, so sums stay complete while the series count stays
bounded. A port MAY choose a different cap, but MUST have one.

A metric point's value is an **integer**: a counter point is a *delta*
observed at its own timestamp, never a cumulative total; a gauge point is
the level at that timestamp. A batching exporter MUST stamp the backend
with the point's own timestamp, never with flush time — a batch delayed by
an incident must not move money in time.

### 5.3 Label fallbacks

Money is never silently lost to a misconfiguration, and cardinality is
never silently blown:

| Situation | Metric label | Event |
|---|---|---|
| `flow` or `stage` not in the registry | the fixed literal `unregistered` | keeps the raw names, for diagnosis |
| `segment` outside the enumeration | the empty string, with a logged warning | keeps the raw value |
| a (`provider`, `op`) pair past the emitter's cap (5.2) | both labels become the fixed literal `other`, with a logged warning | no event — this family is provider health, not a transaction |

All three fallbacks are part of the contract: sums stay complete, the series
count stays bounded, and the misconfiguration is visible on a dashboard.

### 5.4 Ids never become labels

The entity id and the customer id ride **events only**. Any code path that
would place one on a metric is a defect, and an implementation should make
it structurally impossible rather than discourage it. Per-customer
questions are answered from the event sink — which is precisely why a
metrics-only backend must report the customers leg as unavailable (6.3)
rather than empty.

### 5.5 The outcome event

One event per terminal stage transition, per transaction. It carries the
whole value context plus the transition:

| Fact | Notes |
|---|---|
| event time | the *observation's* time — for a webhook-fed adapter, the provider's event timestamp, not receipt time |
| flow, stage, outcome | outcome from the frozen enumeration |
| entity id | the de-dup key; may repeat across attempts, so it is not unique per transaction |
| customer id | already hashed by the caller; never raw PII |
| segment | may be empty |
| amount (minor units), currency, exponent | as in section 1 |
| kind | the money definition |
| estimated | true when the amount came from the registry estimator |
| deadline | when the context carried one |
| trace id | when a trace exists — never load-bearing |
| source | e.g. `stripe:webhook` |
| error text | short, PII-guarded, bounded at 512 bytes |

The **facts** above are the contract, and their serialized key names are
settled: see 7.1 and
[`testkit/vectors/outcome-event.json`](../../testkit/vectors/outcome-event.json),
which is what a port implements against.

### 5.6 The PII guard

Entity id, customer id, source, and error text pass a PII check before an
event is accepted. Error text is the field that matters most: it is where
an upstream provider's message echoes a card number into every sink the
deployment exports to. An implementation MUST reject an outcome carrying
an email address, a PAN, or an IBAN in those fields, and MUST NOT carry
raw PII in any `biz.*` attribute, fixture, or test datum.

The trace id is guarded by shape rather than by inspection: 32 lowercase
hex characters admit no PII by construction.

---
