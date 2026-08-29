---
id: comment-hygiene
severity: MEDIUM
engine: declarative
applies_to: ["**/*.go"]
checks:
  - id: no-bead-ids-in-comments
    pattern: '^\s*(//|/\*).*\bworkspace-[a-z0-9]+(\.[0-9]+)*\b'
    message: "tracked-item ids live in commit headers and PR bodies, never in code comments (CONTRIBUTING, ADR-0014)"
---
The mechanically checkable slice of ADR-0014's comment policy. Tracked-item
/ bead ids do not belong in code comments — CONTRIBUTING places them in
commit headers and PR bodies, and the ADR-0014 sweep removed the 16
instances that had accumulated (a bulk edit, not judged findings — this is
a new prevention check, not a promotion, and it carries no corpus counts).

Scope, stated honestly: the pattern fires on comment-position lines only
(full-line `//` and `/*` starts). A bead id in a trailing comment, a later
block-comment line, or a string literal (the sanctioned user-facing
references in the promql adapter) is out of this check's reach and stays
with the docs-accuracy claude rule and human review.
