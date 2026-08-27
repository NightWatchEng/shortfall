---
id: engine-import-boundary
severity: HIGH
engine: declarative
applies_to: ["engine/**/*.go"]
excludes: ["engine/**/*_test.go"]
checks:
  - id: allowlist-imports
    pattern: '"github\.com/NightWatchEng/shortfall/(?!(?:query|registry|biz|engine)(?:"|/))'
    message: "engine may import only query, registry, biz, and its own subpackages — this boundary keeps the engine backend-neutral"
---
The engine speaks only sum/count/group-by/range through the `query` boundary
and reads flow semantics from `registry`. The check is an ALLOWLIST: any
repo-local import other than `query`, `registry`, `biz`, or an `engine/...`
subpackage fires — including packages that do not exist yet, so a future
`internal/` helper cannot slip in unreviewed.

Test files are excluded: `_test.go` imports (e.g. the testkit conformance
suite) never ship in the compiled engine package and cannot affect
backend-neutrality.

If an engine change appears to need `emit`, an adapter, or anything
vendor-shaped, that is a design smell to fix in the design, not an import to
add. An engine change that genuinely needs a new `Querier` capability is a
frozen-interface amendment: raise it as its own reviewed change.
