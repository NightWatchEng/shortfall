# ADR-0017 — Unavailable marker on report legs (frozen-interface amendment)

Status: accepted (frozen-interface amendment, 2026-08-29)
Date: 2026-08-29

## Context

`engine.Compute` deliberately degrades per leg: a leg its backend cannot
ground carries a caveat or note naming why, instead of failing the whole
report. But the *machine-readable* signal for "this leg was never
measured" was a string convention — a `Caveats`/`Notes` entry starting
with `"unavailable: "` — and not every degradation path even used the
prefix. Renderers that need to distinguish "measured zero" from "never
measured" (the one-line `Summary()` the incident-tool writers PATCH into
vendor impact fields cannot carry caveat prose) had two bad options:
sniff the string format, or render a plausible-looking zero. Review of
workspace-72o found `Summary()` doing the latter — `deferred
[deterministic] none in-flight` for a backend that served no metrics —
and a follow-up round found the same line saying `deferred n/a` and
`unrealized [estimate] none` for the identical degradation class,
because `Unrealized()` degrades via nil-error returns Compute's error
branches never see.

The Request/Report shapes are frozen (v0.1.0), so giving renderers a
structural signal is a reviewed amendment, the route ADR-0009
established for the query surface.

## Decision

- **`Leg.Unavailable bool`** and **`EstLeg.Unavailable bool`**: true
  when the leg could not be grounded at all; a caveat or note names why.
  Renderers must not present an unavailable leg's zero as a measured
  zero. `CustomersLeg` and `CoverageLeg` already carry structural
  markers (`NotAvailableReason`, `Unavailable string`) and are
  unchanged.
- Every degradation path sets it: Compute's error branches for realized,
  deferred, and unrealized, and `Unrealized()`'s own nil-error
  degradation returns (no registry, no metric source, sub-hour window,
  no flow named).
- The addition is purely additive and zero-value compatible: existing
  reports unmarshal with `Unavailable: false`, and a grounded leg is
  unchanged. JSON gains the `Unavailable` keys.
- **`LibraryVersion` bumps to `v0.2.0`** — the field identifies the
  report contract for provenance, and the shape changed.

## Consequences

- `Summary()` renders `realized n/a` / `deferred n/a` / `unrealized n/a`
  off the marker; text and markdown renders carry the caveat/note prose
  itself, so all three human renderers state ungrounded legs honestly.
- The `"unavailable: "` string prefix remains a human-facing convention,
  not a contract; the bool is the contract. Two `Unrealized()` notes
  (sub-hour window, no flow named) keep their prefix-less wording.
- Further Request/Report shape changes follow this same ADR route.
