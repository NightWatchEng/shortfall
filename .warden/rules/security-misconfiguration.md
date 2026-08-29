---
id: security-misconfiguration
severity: HIGH
engine: declarative
implements: [security-misconfiguration]
applies_to: ["**/*.go", "**/*.yml", "**/*.yaml"]
checks:
  - id: tls-verification-off
    pattern: 'InsecureSkipVerify\s*[:=]\s*true'
    message: "TLS peer validation turned off — leave the default on (A02:2025)"
---
Catalog entry A02:2025, adopted 2026-08-29 (workspace-a74) with the
starter adapted from Python (verify=False) to the Go analog. The
debug-enabled check is not carried over: no framework in this tree has a
debug flag, and DEBUG env conventions here are harness-local. Every
adapter dials TLS with defaults today; measured matches at adoption: 0.
A test that genuinely needs a self-signed peer dismisses in one line
with its reason.
