# ADR-0014 — Go readability conventions: line breaks, comment density, doc style

Status: accepted (2026-08-28)
Date: 2026-08-28

## Context

gofmt and go vet are CI-gated and settle layout mechanics, but they decide
neither where a long expression breaks nor how much commentary a file
carries. Both had drifted: dense single-line call sites next to
comment-to-code ratios near 30% in core packages, with inline essays
duplicating ADR content, citing tracked-item ids (which CONTRIBUTING
already banishes to commit headers), narrating change history, and
SHOUTING for emphasis. A library asking for adoption is read far more
often than it is written; its source is part of its API surface.

Go's semicolon insertion makes some line-break choices correctness
issues, not taste: a method chain broken *before* the dot gets a
semicolon inserted after the receiver and silently changes parsing. The
line-break space that gofmt leaves open is mapped in go101's
[Line Break Rules](https://go101.org/article/line-break-rules.html) and
the wider [Go 101](https://go101.org/article/101.html) style chapters;
this ADR picks one convention per open choice.

## Decision

gofmt + go vet remain the mechanical baseline. On top of them:

### Line breaks

- **Method/field chains** that need breaking break **after the dot**,
  never before it — a leading-dot continuation line is a semicolon-
  insertion hazard. Chains short enough to read stay on one line.
- **Calls and composite literals** that exceed the line either stay
  whole or go fully vertical: **one argument or field per line, each
  line ending in a comma** (including the last — the trailing comma is
  what lets gofmt keep the layout). No half-wrapped middle state where
  two args share a line and a third dangles.
- **Long boolean or arithmetic expressions** break **after an
  operator** (`&&`, `||`, `+`, …), so the operator ends the line and
  the continuation is unambiguous under semicolon insertion. Prefer
  naming a sub-condition over a three-line `if`.
- **Soft line target 80–100 columns.** Not a gate; a longer line is fine
  when breaking it reads worse.

### Ordering and grouping

- Imports in three gofmt-separated groups, in order: standard library,
  third-party, this module. No aliasing except to resolve a collision.
- Within a file: package doc, imports, package-level consts/vars, the
  file's primary type with its constructor and methods, then unexported
  helpers. A file is named for its primary type or job.

### Comments

- **Exported identifiers carry a doc comment** (godoc form: starts with
  the identifier's name). State the contract — what the caller may rely
  on — in one to three sentences. Behavioral guarantees (what blocks,
  what is dropped, what is counted) belong here, tersely.
- **A comment inside a function exists only to state a constraint the
  code cannot show** — an invariant, an ordering requirement, a trap for
  the future editor. One or two lines. If the rationale is long, it
  belongs in an ADR; the comment cites it (`// … (ADR-0004).`) instead
  of restating it.
- **Never in comments:** tracked-item / bead ids (CONTRIBUTING: those
  live in commit headers and PR bodies), milestone names, change
  history ("previously…", "the proposal said…"), reviewer persuasion
  ("deliberately", "note that this is correct because…"), restating the
  next line, or ALL-CAPS emphasis. If emphasis is needed, the sentence
  is rewritten until it isn't.
- Duplicated prose is a defect: one rule lives in one place (ADR or doc
  comment) and is cited elsewhere — the same single-source rule
  ADR-0008 applies to docs.

### Review

A diff is reviewable against this ADR: a reviewer may block on a
leading-dot chain break, a half-wrapped call, a bead id in a comment, or
an inline essay duplicating an ADR, citing this ADR by number.

## Consequences

- Comment density drops substantially; what remains is contract and
  invariant, not narration. The application pass shipped with this ADR
  swept every module repo-wide (core packages, adapters, testkit,
  examples, cmd, and the test harness); files it left untouched were
  already compliant. From here the convention holds through review, not
  further sweeps.
- ADR references in code become pointers (`ADR-0004`) rather than
  paraphrases, so an ADR revision no longer strands stale copies of its
  reasoning in comments.
- gofmt is unaffected: everything here lives in the space gofmt leaves
  open, so no tooling changes and no reformat churn.
- Existing merged history is not rewritten; the convention applies to
  new and touched code.
