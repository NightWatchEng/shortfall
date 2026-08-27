# ADR-0001 — Money is int64 minor units + currency + exponent, never float

Status: accepted (ratified at the M2 interface freeze, 2026-08-27)
Date: 2026-08-27

## Context

This library reports dollar figures that Finance must be able to reconcile
against a ledger. IEEE-754 floats accumulate representation error under
summation — exactly the operation the engine performs millions of times per
report — and a coverage ratio computed over drifted sums is a trust number
that lies. Currencies disagree about decimal places (JPY has zero, BHD has
three), so "cents" alone is not a representation either.

## Decision

```go
type Money struct {
    Amount   int64  // minor units: 14900 = $149.00 when Exponent == 2
    Currency string // ISO 4217
    Exponent int8   // decimal places of the minor unit
}
```

- No float32/float64 anywhere in `biz/` — enforced mechanically by the
  `no-float-money` gate rule (types, `strconv.ParseFloat`, and untyped
  decimal literals), and by review charter item 1 for the shapes regexes
  cannot see.
- Multi-currency reports keep native per-currency totals. An optional
  normalized column is fed by a caller-supplied `RateProvider`; the library
  never ships exchange rates and never silently converts.
- Estimated amounts are still integers; uncertainty is expressed by the
  `Estimated` flag and by ranges at the report layer, never by fractional
  precision.

## Consequences

- Summation is exact; reconciliation differences are real differences.
- Callers parsing decimal strings must go through a provided
  `ParseMinor(s, exponent)` helper rather than `ParseFloat`.
- Statistical code (baselines, intervals, recovery fractions) uses floats
  freely — it lives outside `biz/` and produces estimates, which are ranges
  by construction.
