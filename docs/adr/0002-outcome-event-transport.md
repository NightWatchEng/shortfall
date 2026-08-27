# ADR-0002 — Outcome events ride the OTel Log SDK, with a log/slog fallback

Status: proposed (ratify at the M2 interface freeze)
Date: 2026-08-27

## Context

Per-transaction outcome events are the deterministic half of the library
(realized loss, customer impact, reconciliation) and must be emitted
regardless of trace sampling. They need a transport that reaches any
backend. OTLP logs through a collector is the one integration that fans out
everywhere; `log/slog` is the zero-dependency floor every Go shop already
has.

**Stability check at kickoff (2026-08-27, required by this ADR):**
`go.opentelemetry.io/otel/sdk/log` latest is **v0.22.0 — still
experimental (pre-1.0)**, while the core SDK is v1.46.0. The Logs Bridge
API has been stable for some time; the SDK has not reached 1.0.

## Decision

- Outcome events are emitted as OTel log records with
  `event.name = biz.outcome`, a trace-id link when present, and the `biz.*`
  attributes — via the otel-go Log SDK, **isolated inside
  `adapters/export/otlp`** (a nested module), never imported by core
  packages. If the pre-1.0 sdk/log API shifts, the churn is contained in one
  adapter and cannot ripple through `emit` or `engine`.
- The fallback path is a `log/slog` handler emitting the same JSON event
  shape (proposal 4.4) for shops without OTLP; any log pipeline that can
  ship JSON lines becomes an event sink.
- `emit` speaks only the `Exporter` interface; transport choice is
  configuration, not code.

## Consequences

- Core stays zero-heavy-dep; the sdk/log version risk is priced in and
  fenced.
- Re-check sdk/log stability when bumping otel dependencies; move this ADR
  to "accepted, unconditional" once sdk/log tags v1.0.
- The event JSON shape is part of the public contract and versions with the
  library, independent of transport.
