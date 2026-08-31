---
id: docs-accuracy
severity: MEDIUM
engine: claude
implements: [review-lens-documentation]
applies_to: ["**/*.md", "**/*.go", "**/*.sh"]
excludes: [".warden/memory/**"]
covers: [docs-drift, enforcement-claim-drift, scope-misstatement, misleading-comment]
---
Documentation tells the truth (ADR-0008). For Markdown files and code
comments — doc and inline, Go and shell — in this diff, flag when:

- A present-tense claim describes machinery that does not exist in the
  tree (an implementation, rule, suite, CLI verb, or adapter that is
  planned but not landed) without naming its landing milestone or a
  TARGET-design marker.
- A doc claims something is enforced, guarded, or gated and the named
  mechanism is absent or covers less than the sentence says.
- A cross-reference cites a document, symbol, or artifact that does not
  resolve in this repo.
- A behavior or contract change in this diff leaves a README section,
  usage doc, diagram, or doc comment describing the OLD behavior.
- Two statements of one standard (ADR vs CONTRIBUTING vs rule body vs
  comment) disagree after this diff.
- A comment's description of the adjacent code's behavior is contradicted
  by that code — wrong condition, wrong consumer, wrong direction (retro
  2026-08-29: the misleading-comment candidate — 2 distinct findings
  across 3 corpus records, one attested twice on consecutive rounds).

Do NOT flag: clearly future-tensed roadmap text, ADR Context sections
describing history, test names, or planned-and-marked design diagrams.

Evidence: quote the claim AND name the missing/contradicting artifact.

Two mechanical slices are enforced deterministically in the core CI job,
and are NOT judged here:

- Fenced examples — Go must compile, registry YAML must load — by
  test/docsnippets (promoted 2026-08-29): compilation for the docs its
  checkedDocs list governs, registry loading over a slightly wider list
  in its validator test.
- Prose symbol references — a backticked `pkg.Symbol`, `pkg.Type.Member`
  or bare `Type.Member` must resolve against the real packages — by
  test/symbolcheck (workspace-9p1), over README.md, adapters/README.md
  and every .md under docs/ outside docs/adr/.

Their boundary is where this lens resumes. A bare single identifier
(`NotAvailableReason`) is indistinguishable from an ordinary capitalised
word in prose and is not resolved; neither is the TRUTH of a claim built
from names that do resolve — "distinct entities" named a real field and
described the wrong one. Fences and symbols in ADR history stay judged
too.
