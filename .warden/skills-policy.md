# Skills policy — shortfall

The per-project contract the AgentOps skill pack reads (platform
`docs/SKILLS.md` defines the sections). This file is gate surface.

## Verify

- Go changed: `.warden/bin/warden verify --scope core` (gofmt clean, vet,
  build, test across the workspace — nested adapter modules join the scope
  as they land).
- `graph.yaml` changed: `.warden/bin/warden verify --scope graph`.
- Iteration budget before revert: 3 attempts (the platform cap).

## Autonomy scope

- **Merge execution is delegated.** By explicit founder instruction
  (2026-08-27, recorded in `graph.yaml`, revocable there), the session agent
  may merge its own PRs — but only when everything is green: verify scopes
  pass, `warden review --base origin/main --no-comment` exits 0, and the
  pre-PR attestation is CLEAN. Anything red or ambiguous: do not merge,
  report instead. Merge authority itself remains the founder's.
- Never eligible for autonomous work: flipping the repo public (M9 prepares
  it; the flip is the founder's), changing LICENSE, weakening any rule,
  test, benchmark baseline, or verify command to reach green.

## Forbidden paths

Gate surface (`.warden/`, `repo.yaml`, `graph.yaml`, `.github/`,
`.githooks/`) may change only in a PR whose tracked item is explicitly about
policy, CI, or enrollment — never as a side effect of a feature branch. Such
PRs say so in the body.

## Review charter

Money-correctness focus, applied to every review:

1. Float contamination in money paths (`biz/` is int64-only; ADR-0001).
2. Double-count risk: de-dup by entity, retries, multi-provider overlap.
3. Sampling dependence: outcome events must emit regardless of trace
   sampling — any code path that gates money on a sampler is a defect.
4. Cardinality: no unbounded label values on metrics; entity/customer ids
   ride on events only.
5. PII: no email/PAN/IBAN in `biz.*` attributes, fixtures, or test data.
6. Fail-open exporters: an export error that silently drops outcome events
   without incrementing a visible counter is a defect.
7. Realized and estimated value never merged into one number by any
   renderer or consumer.

## Integrations

- dev-executor: none — deliver builds directly in this repo.
- plan-gauntlet: none.
- review-crew: none; the pre-pr-review chain (code-reviewer, then
  cross-examiner as sole arbiter) is the review organization.
- memory: `warden memory ingest` after each merge to main.

## Build disciplines

- test-driven-development: any change under `biz/`, `registry/`, `emit/`,
  `propagate/`, `engine/`, `query/`, or `testkit/` — the failing test comes
  first and is watched failing. Exceptions worth taking without asking:
  doc/comment-only changes, generated code, and skeleton scaffolding that a
  same-milestone item immediately fills with tested behavior.
- systematic-debugging: any red test, flake, or unexpected behavior — name
  the root cause before changing a line. The 3-attempt budget is a stopping
  rule, not a method; an attempt without a named cause is spent.
- verification-before-completion: before any "passes" / "done" claim, have
  fresh output from the full command in hand — never a prior run, never an
  inference from a partial check. A changed hot path reruns its benchmark.

## Shipping

- PR bodies carry: the tracked item id, what was built, verification output
  summary, every review finding with its disposition, and
  `warden attest show`.
- Commits: conventional `type: summary (bead-id)`.
- Merge (delegated, see Autonomy scope): squash, delete branch, close the
  bead, then `warden memory ingest` on main.
