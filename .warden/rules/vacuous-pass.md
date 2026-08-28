---
id: vacuous-pass
severity: MEDIUM
engine: claude
applies_to: ["**/*_test.go", "testkit/**", "test/**", "scripts/**", ".github/**"]
---
A check that can pass without checking anything is worse than no check: it
reads as coverage while asserting nothing. Two of this repo's three known
escapes shipped behind exactly this shape (an empty==empty parity
comparison; a skip that exited 0 under an explicit opt-in).

Flag when this diff adds or touches:

- An assertion that compares two values which can BOTH be empty/zero on the
  same inputs (empty==empty set comparisons, len==len over possibly-empty
  slices) without a guard that the data is non-empty when the scenario
  guarantees data.
- A test or CI step that treats "nothing to do" as success where the caller
  explicitly opted in (an env var, a flag): zero modules found, zero files
  matched, a required tool missing — those exit paths must fail loudly, not
  skip green.
- A loop-and-assert pattern whose loop body can execute zero times with the
  test still passing, when the fixture is meant to guarantee iterations.

Do NOT flag: tests whose empty case is itself the behavior under test (and
named so), or skips gated on an ABSENT opt-in (skipping when the user did
not ask is honest; skipping when they did is the defect).

Evidence: quote the assertion or skip path and state the input shape that
passes it vacuously.
