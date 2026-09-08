# ADR-0020 — baseline, recovery and reconcile are optional per flow; a leg without its block is unavailable, never guessed

Status: accepted (2026-09-08)
Date: 2026-09-08

## Context

The registry has required every flow to declare three blocks — `baseline`,
`recovery` and `reconcile` — on the argument that the legs which consume
them must not silently guess: the baseline is how "demand that never
arrived" is sized, recovery is what fraction of it comes back, and the
reconcile source names the ledger the trust number is measured against.
The quickstart therefore carried all three in its one-stage hello world
and told the reader the values were placeholders to tune later.

The 2026-09-03 review of the first-run experience found that this is
three Finance concepts before a single number appears, and that the rule
they guard is already enforced elsewhere by a different mechanism:
ADR-0017's `Unavailable` marker. The deferred leg on an events-only
backend, the coverage leg at impact time, and the unrealized leg on a
querier with too little history all say what they cannot ground rather
than guessing — and none of them needs the validator to force a block
into the file to do it.

## Decision

- **The three blocks are optional.** A flow with no `baseline`, no
  `recovery` and no `reconcile` block loads. A block that is present —
  `baseline: {}` included — is validated in full, exactly as before: an
  empty block is a typo, and the way to declare no block is to omit it.
- **No baseline: the unrealized leg is unavailable, naming the block.**
  The leg is now marked `Unavailable` whenever no requested flow could be
  sized, which it was not before (it returned empty maps a renderer could
  print as zero).
- **No recovery: the estimate is gross, and the note says so.** Nothing is
  credited back, and the leg's note states that the flow declares no
  recovery model — distinct from a declared `recovered_fraction: 0`, which
  is a decision and gets no note.
- **No reconcile: nothing changes at impact time.** Coverage is a
  reconcile-time number (ADR-0011, ADR-0017) whose ledger arrives on the
  `shortfall reconcile` command; the block is where Finance names that
  ledger and, optionally, the value stage (ADR-0016). Without it the value
  stage is the last stage, as before. A present block that names no source
  is rejected: the portability contract's `reconcile_source_required`
  class narrows from "a flow with no reconcile source" to "a reconcile
  block that names no source".
- **The contract's minimal document shrinks** to `version`, `segments`,
  and one flow with `money` and `stages`; the `minimal` acceptance vector
  carries it and its facts.

## Consequences

- The quickstart registry is money and stages. The legs that need more say
  so in the report rather than in a validator error about a ledger the
  reader has not chosen.
- A flow that forgets its baseline learns it from the unrealized line of
  its first report, not at startup; `shortfall validate` still runs, and
  the line is loud.
- Every shipped registry example keeps the three blocks: they are the
  shape a Finance-reviewed registry has. Only the requirement moved.
- Ports MUST accept the minimal document and MUST still reject a present,
  empty block under the existing classes.
