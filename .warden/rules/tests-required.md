---
id: tests-required
severity: MEDIUM
engine: claude
implements: [review-lens-tests]
applies_to: ["biz/**", "registry/**", "emit/**", "propagate/**", "engine/**", "query/**", "testkit/**", "examples/**", "cmd/**", "adapters/**", "test/**"]
---
Definition of Done: new logic carries unit tests in the same diff; bugs found
live or in review get a NAMED regression test; a changed hot path
(Baggage codec, emit.Record, in-flight bucketing, engine Compute, baseline
fit) keeps its benchmark honest. Unit tests are TABLE-DRIVEN per ADR-0007:
named cases run via t.Run subtests, one uniform assertion body. The only
exempt shapes are ADR-0007's declared exceptions (fuzz targets, property
loops, golden/drift fences, end-to-end scenario tests, benchmarks).

Flag when this diff:
- Adds a function or branching logic under a core package with NO
  corresponding `_test.go` change in the same diff.
- Is clearly a bug fix but adds no regression test naming or covering the bug.
- Changes a benchmarked hot path without touching (or justifying not
  touching) its benchmark.
- Adds a multi-case unit test that is not table-driven with t.Run-named
  subtests, and it is not one of ADR-0007's declared exception shapes.

Do NOT flag: pure refactors with existing coverage, doc.go/comment-only
changes, skeleton stubs a same-milestone item immediately fills with tested
behavior, or config/docs changes.

Evidence: name the new/changed symbol and note that no matching test change
exists.
