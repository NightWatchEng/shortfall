---
id: engine-import-boundary
severity: HIGH
engine: declarative
applies_to: ["engine/**/*.go"]
checks:
  - id: no-side-imports
    pattern: '"github\.com/Nigthwatch-eng/shortfall/(emit|propagate|adapters|cmd|examples|testkit)'
    message: "engine may import only query, registry, and biz — this boundary is what keeps the engine backend-neutral"
---
The engine speaks only sum/count/group-by/range through the `query` boundary
and reads flow semantics from `registry`. If an engine change appears to need
`emit`, an adapter, or anything vendor-shaped, that is a design smell to fix
in the design, not an import to add.

An engine change that genuinely needs a new `Querier` capability is a frozen-
interface amendment: raise it as its own reviewed change, never smuggle it in
via an import.
