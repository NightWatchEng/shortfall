# ADR-0011 — Coverage ratio: worst-slice value coverage; per-slice deltas attributed

Status: accepted (2026-08-28)
Date: 2026-08-28

## Context

The coverage leg is the trust line every report carries: how much of the money
the provider's books (the reconciled ledger) recorded did our TELEMETRY also
see. A report without a trust line is a claim, not a measurement — a dropped
exporter or a mis-scoped query silently understates loss, and Finance has no
way to know.

`engine.CoverageLeg` (frozen at M2) carries a SINGLE `Ratio float64`. Coverage
compares money — telemetry success value vs ledger success value — but ADR-0001
forbids summing value across currencies, so a single cross-currency value ratio
is not well-defined. A design is needed that yields one honest scalar while
respecting the currency invariant.

## Decision

- Coverage is computed **per slice** = per (flow, currency): `ratio_slice =
  telemetry_success_value / ledger_success_value`, clamped to `[0, 1]`
  (telemetry seeing MORE than the ledger is capped at full coverage, not
  >100% — the ledger is the denominator of record).
- The report's single `Ratio` is the **minimum slice ratio** — the
  worst-covered (flow, currency). A trust number is a weakest-link number: if
  any slice is poorly reconciled, Finance should see the low figure, not an
  average that hides it. For the common single-slice case this is simply that
  slice's value ratio.
- The **delta is attributed per slice**: the reconcile job reports, for each
  (flow, currency), telemetry value, ledger value, and the shortfall — so a
  sub-100% number names exactly where telemetry and the ledger diverge. This
  attribution is the reconcile CLI's output; the frozen `CoverageLeg` is not
  amended (its `Source` names the ledger source, its `Ratio` the worst slice).
- A slice present in the ledger but ABSENT from telemetry counts as 0 coverage
  for that slice (the exporter saw nothing) — this is exactly the dropped-
  exporter case the leg exists to catch. A slice in telemetry but not the
  ledger does not lower coverage (the ledger is the denominator) and is OUT OF
  SCOPE for v0 attribution, which reports the ledger's slices; detecting a
  telemetry-side over-count (a duplicating exporter) is future work, not a
  guarantee this leg makes.
- A ledger slice whose value sums to zero is SKIPPED, not scored 0: coverage of
  zero value is undefined, and a legitimate $0 success slice must not tank the
  headline.
- When there is no ledger — or no ledger slice with value — to compare against,
  the leg is `Unavailable`, never a fabricated 100%.

## Consequences

- Deterministic and ADR-0001-safe: no value is ever summed across currencies;
  the scalar is a min of same-currency ratios.
- The headline is conservative by construction — it cannot look better than the
  weakest slice — which is the right bias for a number Finance signs.
- Coverage over transaction COUNTS was considered and rejected as the headline:
  a count ratio hides value-weighted gaps (a dropped exporter that drops a few
  high-value transactions barely moves a count ratio). Counts may still appear
  in the per-slice attribution.
- A future need for a value-weighted blended figure across currencies would
  require an FX policy the library deliberately does not have (ADR-0001), so it
  stays out of scope.
