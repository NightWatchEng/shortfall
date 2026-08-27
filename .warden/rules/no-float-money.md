---
id: no-float-money
severity: HIGH
engine: declarative
applies_to: ["biz/**/*.go"]
checks:
  - id: no-float-types
    pattern: 'float(32|64)'
    message: "money is int64 minor units + currency + exponent — floats drift (ADR-0001)"
---
Money in this library is `int64` minor units with a currency and exponent,
never a float. Floats drift, and drift is exactly what ledger reconciliation
exists to catch — a library that reports dollar figures to Finance cannot
carry rounding error in its core types.

This rule scopes to `biz/` only: statistical code (baseline intervals, MAD,
recovery fractions) legitimately uses floats and lives elsewhere. If a float
genuinely belongs in `biz/`, that is an ADR-0001 amendment, not an exception.
