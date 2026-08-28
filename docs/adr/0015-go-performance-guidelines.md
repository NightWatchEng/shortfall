# ADR-0015 — Go performance guidelines: a reviewed reference checklist for hot paths

Status: accepted (2026-08-28)
Date: 2026-08-28

## Context

This library runs inside adopting services' request paths, so performance
is part of its contract (CONTRIBUTING "Benchmarks"). The hot paths are
benchmarked — the Baggage codec (`BenchmarkEncodeVC`/`DecodeVC`),
`emit.Record` (`BenchmarkRecordAccept`/`RecordSuppressed`), in-flight
bucketing (`BenchmarkAgeBucketFor`, `BenchmarkTrackerPublish10k`), engine
`Compute`, and the baseline fit (`BenchmarkExpected`) — and CI compares
every PR against main with benchstat.

What was missing is a shared reference for *how* to optimize when a
benchmark says to: without one, optimization PRs argue technique from
scratch, and reviewers have no standard to judge cleverness against. The
techniques below are drawn from go101's Go Optimizations 101
(https://go101.org/optimizations/101.html).

## Decision

This ADR is a **reference checklist, not a mandate**. The order of
authority is fixed:

1. **Correctness first** — an optimization that changes observable
   behavior is a bug.
2. **Readability second (ADR-0014)** — an optimization that obscures the
   code needs a benchmark delta large enough to justify itself, stated in
   the PR body.
3. **The benchstat comparison arbitrates** — a claimed optimization that
   does not move its benchmark is a readability regression with no payer,
   and a change that regresses a hot-path baseline carries a stated
   reason in the PR body (CONTRIBUTING: a review convention — the CI job
   is advisory, ratcheting toward required as baselines stabilize).
   Optimizations are proposed for benchmarked hot paths; cold paths take
   the simplest correct form.

### The checklist

Consult when a hot-path benchmark needs improving, in roughly the order
of typical yield:

- **Allocation placement / escape analysis.** The biggest wins are
  usually allocations that stop happening: check `go build -gcflags=-m`
  before restructuring. Prefer value receivers/locals that stay on the
  stack; avoid capturing locals in closures that escape; reuse buffers
  the profile says are hot (the emitter's two-generation de-dup set
  rotates by clearing and reusing its maps for exactly this reason).
- **Preallocation and reuse.** Size slices and maps at creation when the
  final size is known or bounded (`make([]T, 0, n)`, `make(map[K]V, n)`);
  `append` growth and map rehashing are the quiet cost centers. Reuse via
  `clear()` rather than reallocating in loops.
- **String/[]byte conversions.** Each conversion copies. Hoist repeated
  conversions out of loops; build strings with `strings.Builder` sized by
  `Grow`. Do not reach for `unsafe` aliasing — no benchmark here has
  justified it, and it breaks the readability order above.
- **Struct field ordering.** Order fields to minimize padding when a
  struct is allocated in bulk (slices of points, per-event records) —
  alignment waste multiplies by element count. Irrelevant for singleton
  config structs; do not churn those.
- **Interface boxing.** Storing a non-pointer value in an interface
  allocates. On hot paths, keep concrete types in the inner loop and box
  at the boundary once, not per element.
- **Bound-check elimination.** In proven-hot loops, hoist a single
  `_ = s[len(s)-1]` style check or iterate with `range` so the compiler
  drops per-access checks; verify with `-gcflags=-d=ssa/check_bce`.
- **Inlining.** Small leaf functions on hot paths inline automatically;
  keep them small rather than annotating. Never contort a function's
  shape purely to stay under the inlining budget without a benchmark
  showing it matters.

### Process

- An optimization PR names the benchmark it targets and quotes the
  benchstat delta (old → new) in the PR body.
- A reviewer may reject an optimization by citing this ADR's order of
  authority: no delta, no cleverness.
- Micro-optimizing unbenchmarked code is declined as scope creep; if the
  path matters, the benchmark lands first (CONTRIBUTING's hot-path list
  grows with it).

## Consequences

- No code changes from this ADR itself; the existing benchmarks and the
  benchstat CI job remain the only performance gate.
- Optimization discussions gain a fixed vocabulary and burden of proof:
  the technique list is pre-agreed, so PR review argues numbers, not
  folklore.
- go101's Optimizations 101 is the canonical elaboration; this ADR stays
  a checklist and does not duplicate its reasoning (the same
  single-source rule as ADR-0008/0014).
