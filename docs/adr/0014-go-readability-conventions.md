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
- **A blank line follows the brace that closes a block.** After the `}`
  that closes a block-bodied statement — `if`, `for`/`range`, `switch`, a
  type switch, `select`, with or without a label — a blank line separates
  it from whatever follows at the same level. Four exceptions, and no
  others: `else`/`else if` continuing the same `if`; `case`/`default`
  opening the next clause; the block being the last statement in its list,
  so what follows is the enclosing `}` (the end of a function body
  included); and a block written entirely on one line, which has no
  closing brace of its own to break after. A comment counts as the code
  that follows, so the blank line goes before it. A `defer` or `go` with a
  function literal, a multi-line composite literal, and a wrapped call all
  end in a brace or paren without being blocks; they are outside the rule.

  **Amended 2026-08-31 (workspace-16a)**: this section decided horizontal
  breaking only — chains, argument wrapping, operator placement, the
  80–100 soft target — and said nothing about vertical whitespace, which
  left the commonest layout decision in Go source to taste. The bullet
  above is new; no text that was already here changed. It is enforced
  deterministically by `test/blankline`, a module modelled on
  `test/licensehdr`: `go run ./test/blankline` lists violations as
  `file:line`, `-fix` inserts the blank lines in place, and a test in the
  module fails the ordinary test step for any tracked `.go` file that
  violates the rule — so the convention rides the required `core checks`
  job with no gate wiring. The checker parses with `go/parser` and
  compares `token.Position` lines rather than matching text, which is why
  a composite literal and a one-line `if err != nil { return err }` cannot
  be mistaken for blocks. It excludes nothing: every tracked `.go` file is
  checked, generated and test files included.

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
  **Amended 2026-08-31 (workspace-16a)**: "not further sweeps" was written
  about the comment and line-break conventions this ADR accepted, and it
  held for them. The blank-line clause added to the Line breaks section
  above arrived later and needed its own application pass: 2,307 blank
  lines inserted across 149 tracked files, by `go run ./test/blankline
  -fix`. A third sweep for that clause cannot be needed — unlike the
  conventions this bullet describes, it has a checker in the required test
  step, so a violation fails before it merges rather than accumulating.
- ADR references in code become pointers (`ADR-0004`) rather than
  paraphrases, so an ADR revision no longer strands stale copies of its
  reasoning in comments.
- gofmt is unaffected: everything here lives in the space gofmt leaves
  open, so no tooling changes and no reformat churn.
  **Amended 2026-08-31 (workspace-16a)**: "no tooling changes" held for
  the conventions accepted here, which carry no checker. The blank-line
  clause added above does carry one, `test/blankline`; the first half of
  this bullet is unchanged, because a single blank line between two
  statements is exactly what gofmt preserves, so the swept tree is
  gofmt-clean and the `fmt` check saw no work.
- Existing merged history is not rewritten; the convention applies to
  new and touched code.
