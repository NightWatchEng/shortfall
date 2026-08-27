---
id: no-float-money
severity: HIGH
engine: declarative
applies_to: ["biz/**/*.go"]
checks:
  - id: no-float-types
    pattern: '^\s*(?!//).*float(32|64)'
    message: "money is int64 minor units + currency + exponent — floats drift (ADR-0001)"
  - id: no-parse-float
    pattern: '^\s*(?!//).*strconv\.ParseFloat'
    message: "parsing money through float loses cents — parse minor units as int64 (ADR-0001)"
  - id: no-untyped-float-literal
    pattern: '^\s*(?!//).*:=\s*-?\d+\.\d'
    message: "untyped decimal literal defaults to float64 — money literals are int64 minor units (ADR-0001)"
---
Money in this library is `int64` minor units with a currency and exponent,
never a float. Floats drift, and drift is exactly what ledger reconciliation
exists to catch — a library that reports dollar figures to Finance cannot
carry rounding error in its core types.

The three checks catch the common entry paths: explicit float types,
`strconv.ParseFloat`, and untyped decimal literals (which default to
float64). Pure `//` comment lines are carved out so documenting the ban
cannot trip it. The checks are mechanical and therefore incomplete — a float
that arrives through a function return type or an interface still lands on
review charter item 1, which every review applies.

This rule scopes to `biz/` only: statistical code (baseline intervals, MAD,
recovery fractions) legitimately uses floats and lives elsewhere. If a float
genuinely belongs in `biz/`, that is an ADR-0001 amendment, not an exception.
