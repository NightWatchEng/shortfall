# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities privately via GitHub's security advisory
form for this repository ("Report a vulnerability" under the Security tab).
Please do not open public issues for security reports. You should receive
an acknowledgement within a week; fixes ship as ordinary patched releases
with credit if you want it.

## Scope notes for this library

- **Webhook signature verification** (`adapters/payment/stripe`): the
  receiver verifies provider signatures before an event becomes an
  outcome. Bypasses of that verification, or ways to forge outcomes into
  the money paths, are in scope and high priority.
- **PII containment**: `biz.*` attributes are guarded against email, PAN,
  and IBAN shapes by code (`biz.CheckPII`), and amounts/ids ride events
  only. Ways to smuggle cardholder data or PII past the guard are in
  scope.
- **Value-context propagation** (`biz.vc` Baggage member): the egress
  allowlist keeps amounts and customer hashes from riding to third
  parties. Allowlist bypasses are in scope.
- **Secrets**: adapters carry credentials in headers only; anything that
  causes a token to be logged, persisted, or echoed is in scope.

## Supported versions

Pre-1.0: only the latest v0.x release receives security fixes.
