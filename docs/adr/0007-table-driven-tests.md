# ADR-0007 — Table-driven tests are the repository standard

Status: accepted (founder mandate, 2026-08-27)
Date: 2026-08-27

## Context

Go's testing culture converged on table-driven tests for a reason the Go
wiki states plainly: given a table of cases, adding a case is one line,
every case is named, failures identify exactly which input broke, and the
assertion logic exists once instead of being copy-drifted. This repo's
review history shows the alternative failure modes concretely: loop tests
whose failure message needs archaeology, single-scenario tests that pass
for the wrong reason, and coverage gaps invisible because cases live in
prose rather than a table a reviewer can scan for holes.

## Decision

**Every unit test that asserts input/output behavior MUST be
table-driven**, in the standard Go shape:

```go
cases := []struct {
    name string        // or use the input itself when self-describing
    in   ...           // inputs
    want ...           // expected outputs / error expectation
}{ ... }
for _, c := range cases {
    t.Run(c.name, func(t *testing.T) {
        // one assertion body, no per-case logic forks
    })
}
```

Binding details:

- Cases are **named** and run via **`t.Run` subtests** — a bare loop with
  `t.Errorf("%s: ...", name)` does not satisfy this ADR: subtests give
  `-run 'Test/case'` selection, per-case failure isolation, and honest
  counts.
- The assertion body is **uniform**: a case that needs its own special
  assertion logic is a different test, not a table row with an if-ladder.
- Rejection tables assert **more than "an error happened"** where the
  error contract matters (this repo's registry tables assert the error
  names the offending field).

**Declared exceptions** — shapes where a table cannot apply. Using one is
fine; the test says which it is when it is not obvious:

1. **Fuzz targets** (`Fuzz*`) and **property loops** (e.g. the 1M-iteration
   codec round-trip): the case generator is the point.
2. **Golden/drift fences** (testkit's goldens-match-ground-truth): one
   comparison against committed truth.
3. **End-to-end scenario tests** (the checkout harness fault scenarios):
   one simulated world, many coherence assertions over it.
4. **Benchmarks**.

A unit test that is genuinely a single case (one construction, one
assertion) may stay a single case — a one-row table is ceremony, not
rigor — but the moment a second case appears, the table does.

## Enforcement

- The `tests-required` review rule (engine: claude) names this standard;
  the pre-PR review flags non-table-driven unit tests as findings.
- CONTRIBUTING carries the contract for humans.
- Existing suites were audited and converted in the PR that lands this
  ADR; the exception classes above cover what was deliberately left.

## Consequences

- Reviewers scan tables for missing rows instead of re-deriving coverage.
- `go test -run Test/name` reproduces any single failing case.
- The cost is boilerplate on trivial tests, accepted deliberately: the
  repo optimizes for the 2am reader, and the 2am reader wants the failing
  case's name in the output.
