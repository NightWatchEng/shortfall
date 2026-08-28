---
id: vacuous-pass
severity: MEDIUM
engine: claude
applies_to: ["**/*_test.go", "testkit/**", "test/**", "scripts/**", ".github/**"]
---
A check that can pass without checking anything is worse than no check: it
reads as coverage while asserting nothing. The first draft of this repo's
live parity gate was vacuous in exactly two ways — an empty==empty gauge
comparison, and an opt-in skip that exited 0 when Docker was missing — and
either shape would have hidden the two known escapes (PRs #32, #37) the
gate existed to catch. Both were caught in that PR's (#55) own review; the
corpus carries 9 judged findings of this class.

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
