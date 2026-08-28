# ADR-0009 — EventQuery representative-per-group (max) for exact per-entity de-dup

Status: accepted (frozen-interface amendment, 2026-08-28)
Date: 2026-08-28

## Context

The realized leg (and, when it lands, the coverage leg) de-duplicates failed
value by entity: an entity that failed is counted once, and an entity that also
succeeded in the window is excluded (it recovered). To collapse the duplicate
failed events one entity can carry — the same failed event redelivered under
at-least-once transport — the engine grouped by `(currency, entity)` and read
`SumMinor / Count`.

That mean is **exact only when the duplicates are identical** (a redelivered
event carries the same amount). When an entity has failed events with
*differing* amounts — partial captures, corrections, or order-id reuse — the
mean is not a real figure, and when it happens to divide evenly (100 + 300 over
two events = 200) the inconsistency is not even detectable from a sum-and-count
result. There is no "entity id is unique per transaction" invariant in the
library: `biz.ValueContext.EntityID` is just an invoice/order id, and the
recovery path deliberately relies on one entity carrying both failed and
success outcomes.

The frozen `query.EventQuery` (v0.1.0) returned only `SumMinor` and `Count` per
group, so the engine had no way to take one *representative* amount per entity.
Exact de-dup needs a new query capability. Because the query surface is frozen,
this is a reviewed amendment, per the engine-import-boundary discipline
(tracked as workspace-7y5).

## Decision

Add one aggregation and one result field to `query.EventQuery`:

- **`EventAggMaxPerGroup`** (`"max_per_group"`): like `EventAggGroups`, returns
  one group per distinct key, but each group additionally carries the
  **maximum single event's minor amount** in the group.
- **`EventGroup.MaxMinor`**: the representative amount, populated **only** when
  `Agg == EventAggMaxPerGroup`. `Count` and `SumMinor` are still populated.

The representative is the **maximum**, not the mean:

- For identical redeliveries it equals the value — **exact**, which is the
  common case the mean also got right.
- For genuinely differing failed amounts on one entity it takes the **largest
  single failed attempt** — a real, observed, deterministic figure (the
  worst-case exposure), never a synthetic average.

`MaxMinor` is money, so it is subject to the **same currency invariant** as
`SumMinor`: when `Agg == EventAggMaxPerGroup`, `currency` must be in `GroupBy`
or pinned in `Filters`, or the adapter rejects the query (ADR-0001 — no silent
cross-currency comparison). Every event backend can serve it (`MAX(amount)` in
SQL, a running max in memq); it needs no new `Caps` bit.

The realized leg now groups `(currency, entity)` with `EventAggMaxPerGroup` and
sums each group's `MaxMinor`, so per-entity de-dup is exact for redeliveries and
deterministic for differing amounts — the `SumMinor/Count` mean and its
"inconsistent amounts" caveat are removed.

## Consequences

- `EventGroup` gains a field and `EventAgg` gains a value; both are additive and
  backward compatible (existing callers that never set `EventAggMaxPerGroup`
  see `MaxMinor == 0` and unchanged behavior). This is the only amendment to the
  frozen query surface since the freeze; further ones follow the same ADR route.
- MAX chooses worst-case exposure over recency. If a future need calls for the
  first/last-per-group representative instead, that is a further ADR — it needs
  in-group ordering (a window function in SQL), which MAX does not.
- memq and the SQL adapter implement `max_per_group`; a conformance case pins
  them to the same result. Metrics-only adapters (PromQL) are unaffected — they
  serve no events.
