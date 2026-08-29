---
id: sql-injection
severity: HIGH
engine: declarative
implements: [sql-injection]
applies_to: ["**/*.go"]
checks:
  - id: sprintf-into-query
    pattern: '(Query|Exec|Prepare)(Context)?\(\s*(ctx\s*,\s*)?fmt\.Sprintf'
    message: "SQL built inline with Sprintf at the call site — pass placeholders and args (CWE-89)"
  - id: concat-into-query
    pattern: '(Query|Exec|Prepare)(Context)?\(\s*(ctx\s*,\s*)?"[^"]*"\s*\+'
    message: "SQL text concatenated at the call site — pass placeholders and args (CWE-89)"
---
Catalog entry CWE-89, adopted 2026-08-29 (workspace-a74) with the
starter adapted from Python (execute(f"...")) to the database/sql call
shapes. Scope, stated honestly: the checks catch inline construction at
the call site only — SQL assembled in a variable first (the sql
adapter's own pattern) is beyond any regex and stays with review; that
adapter is safe by design (bare-identifier table validation, values
always parameterized) and the conformance suite pins it. Measured
matches at adoption: 0.
