# ADR-0019 — The deferred leg is groundable from outcome events (ADR-0005/0012 amendment)

Status: accepted (2026-09-08)
Date: 2026-09-08

## Context

The deferred leg — money in flight, the README's answer to "delayed money
is not lost money" — has had exactly one grounding: the `biz_inflight_value`
gauge (ADR-0005) and its companion count (ADR-0012), which an
`emit.InFlightTracker` publishes from an in-process set it maintains
between `Track` and `Done`. Everything the leg reports is derived from the
last level of that gauge inside the window.

That grounding assumes the process that enqueued the work is the process
that will see it finish, and that it is alive at the snapshot. Neither
holds in the deployments the integration guide describes. A producer and
its consumer are different processes; a consumer stall or crash is exactly
when the tracker stops publishing, so the gauge goes stale or absent at
the moment the leg matters; a Lambda cannot host a tracker at all. The
worked webhook example records `ResultDeferred` on a payments-service 5xx,
and that outcome reached nothing: the engine read `deferred` events for a
transaction count and nothing else.

Meanwhile the outcome event already survives process death, is unsampled
by contract (ADR-0002), and is what the realized and customer legs stand
on. It carries the flow, stage, entity, amount and time of every
`deferred` transition an emitter records.

## Decision

- **A second grounding path, from events.** An entity is in flight at the
  window end when it has a `deferred` outcome in scope and, within the
  lookback, no terminal outcome — `success`, `failed` or `abandoned` — at
  the same stage or any later stage of the flow's registry order, and no
  further `deferred` outcome at a later stage (entering the next queue
  means the earlier one released it). A terminal at an earlier stage
  resolves nothing: a settle deferral outlives its capture success. Its
  value is the largest single `deferred` amount per (currency, entity,
  stage), the representative rule ADR-0009 fixed for realized loss.
  `unknown` is not terminal: the money is still unresolved. "Later" is
  by stage order only — the event AST returns no per-event times — so a
  deferral retried after a failure at the same stage is not seen, and the
  leg's caveat says so.
- **Age without a new verb.** The query surface is frozen (v0.1.0) and
  the event AST returns no per-event timestamps. Buckets are derived from
  nested ranges instead: the deferred query runs over
  `[lookback, To-2h]`, `[lookback, To-30m]`, `[lookback, To-5m]`,
  `[lookback, To-1m]` and `[lookback, To)`, each cut inclusive of its
  instant so that an entity exactly thirty minutes old is `30m-2h` as
  `emit.AgeBucketFor`'s left-closed intervals have it, and an entity's
  bucket is the oldest range its first `deferred` event falls in. The SLA-breach test,
  projected-lost arithmetic, exact count and oldest-age floor then reuse
  the bucket-floor code the gauge path already has, unchanged. The
  lookback is the larger of two hours and the flow's longest SLA
  deadline, so backlog older than the window start is still seen.
- **Precedence.** A metrics backend that returns any `biz_inflight_value`
  series for the scope — a level of zero included — is authoritative and
  the events path does not run: a tracker that observed `Done` knows about
  completions the event window may not. A metrics backend with no
  in-flight series, or an events-only backend, grounds the leg from
  events. A backend serving neither stays unavailable (ADR-0017).
- **Evidence and caveat.** The events-derived leg is deterministic — every
  amount is one recorded transaction's — and carries one caveat naming the
  source and its limits: resolution is by stage order, not time; age is
  measured from the first `deferred` event; and work an emitter never
  recorded as `deferred` is unseen. The gauge path carries no caveat.
- **Nothing on the wire changes.** `ValueContext.Deadline` stays encoded
  (ADR-0003's codec is unchanged) and stays unread by the engine; this
  ADR records that rather than leaving it implied.
- **The reference harness publishes through the real tracker.** The
  goldens' in-flight gauge points come from replaying the simulation
  ledger through `emit.InFlightTracker` with a fixed clock, not from a
  hand-written re-derivation of its bucketing, and the harness records the
  `deferred` outcome at each queue enqueue as the integration guide
  instructs a real emitter to.

## Consequences

- An events-only backend (`adapters/query/sql`, `cwinsights`,
  `gcplogging`) now grounds three legs instead of two; the adapters
  matrix and the worked example say so.
- A `deferred` outcome an emitter records at enqueue is now load-bearing:
  the integration guide's "record `ResultDeferred` when a stage goes
  async" is what makes a queue outage visible when the tracker cannot be.
- Bucket granularity is the same floor the gauge path has (ADR-0005,
  ADR-0012): a deadline inside a bucket is not attributed, and a deadline
  past the `gt2h` floor never registers as breached. Exact ages would need
  a per-group timestamp aggregation on the query surface — a frozen-AST
  amendment left for a flow that needs it.
- Five bounded event queries per flow, and only on the path where no gauge
  grounded the leg.
- A broker-depth in-flight source (queue attributes, consumer lag) remains
  a separate question: it yields counts, not money, and needs its own
  record.
