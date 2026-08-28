---
id: docs-accuracy
severity: MEDIUM
engine: claude
applies_to: ["**/*.md", "**/*.go"]
excludes: [".warden/memory/**"]
covers: [docs-drift, enforcement-claim-drift, scope-misstatement]
---
Documentation tells the truth (ADR-0008). For Markdown files and Go doc
comments in this diff, flag when:

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

Do NOT flag: clearly future-tensed roadmap text, ADR Context sections
describing history, test names, or planned-and-marked design diagrams.

Evidence: quote the claim AND name the missing/contradicting artifact.
