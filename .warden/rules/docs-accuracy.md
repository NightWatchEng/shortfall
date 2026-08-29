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

The mechanical slice — fenced Go examples must compile, fenced registry
examples must load — is enforced deterministically by test/docsnippets
in the core CI job (promoted 2026-08-29): Go compilation for the docs
its checkedDocs list governs, registry loading over a slightly wider
doc list in its validator test. Fences elsewhere (docs/inhouse.md until
it is governed, ADR history) stay with this lens's judgment.
