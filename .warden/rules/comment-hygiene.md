---
id: comment-hygiene
severity: MEDIUM
engine: declarative
applies_to: ["**/*.go"]
checks:
  - id: no-bead-ids-in-comments
    pattern: '//.*\bworkspace-[a-z0-9]+(\.[0-9]+)*\b'
    message: "tracked-item ids live in commit headers and PR bodies, never in code comments (CONTRIBUTING, ADR-0014)"
---
The mechanically checkable slice of ADR-0014's comment policy, promoted from
the docs-accuracy corpus (n=51, wilson_lb=0.897): tracked-item / bead ids do
not belong in code comments — CONTRIBUTING places them in commit headers and
PR bodies, and the ADR-0014 sweep removed the ~15 instances that had
accumulated. This check keeps them from coming back. The judgment half of
the policy (essay comments, change narration, persuasion wording) stays with
the docs-accuracy claude rule and human review.
