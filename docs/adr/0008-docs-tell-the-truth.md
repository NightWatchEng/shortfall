# ADR-0008 — Documentation tells the truth

Status: accepted (founder mandate, 2026-08-27)
Date: 2026-08-27

## Context

This repo's review corpus makes the case empirically: the single
most-confirmed finding class here is documentation drift — rule bodies
promising coverage a checker does not deliver, ADRs citing artifacts that
do not exist, present-tense claims for machinery landing milestones
later, comments contradicting the code beside them, and diagrams drawing
planned components as shipped. Twelve-plus confirmed instances before
this ADR, including in the PR that landed the architecture diagrams
themselves. Docs that overclaim are worse than no docs: a reader acts on
the claim, and the gap surfaces at 2am.

## Decision

Reader-facing documentation — README, usage docs, CONTRIBUTING,
`docs/architecture` diagrams, ADRs, package doc comments, and gate rule
bodies — MUST satisfy:

1. **Honest tense.** What exists is stated as existing; what is planned
   is named with its landing milestone or marked TARGET design. A
   present-tense claim about unbuilt machinery is a defect, not style.
2. **Enforced means enforced.** A doc may claim a rule, guard, or gate
   only if the mechanism exists and covers what the sentence says. Where
   a mechanism is partial, the doc states the gap (the pattern set by the
   secrets rule: actual coverage listed, known gaps named).
3. **References resolve.** Every cross-reference points at something in
   the repo. Citing an external proposal or absent document as the
   definition of a contract is a defect — inline the contract.
4. **Same-PR updates.** A PR that changes behavior, architecture, or a
   contract updates the affected README section, usage doc, and diagram
   in that PR. Stale-doc debt is not a follow-up; it is an incomplete PR.
5. **One standard, one wording.** When the same rule is stated in
   multiple documents (ADR + CONTRIBUTING + rule body), the statements
   agree; divergence is a defect in whichever drifted.

## Enforcement

- The `docs-accuracy` review rule (engine: claude) applies to every
  Markdown file and to Go doc comments; the pre-PR review flags
  violations. It declares `covers: [docs-drift, enforcement-claim-drift]`
  so the corpus routes those recurring classes to it.
- The review charter carries the check so every review applies it even
  when no rule fires.
- The diagrams' Definition-of-Done rule (CONTRIBUTING) is subsumed and
  broadened by clause 4.

## Consequences

- Doc edits ride behavior PRs, so reviews judge code and its description
  together — where drift is cheapest to catch.
- Writing "lands in M6" costs one clause and buys a reader who can trust
  every unqualified sentence.
- Amendment trail: this ADR's first application reconciled the
  event-transport ADR's drop-reason enum with the emitter contract (the
  enum gained `invalid`), a mismatch the diagrams review surfaced.
