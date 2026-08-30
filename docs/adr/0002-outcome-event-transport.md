# ADR-0002 — Outcome events ride the OTel Log SDK, with a log/slog fallback

Status: accepted (ratified at the M2 interface freeze, 2026-08-27)
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
- The **canonical event shape** is defined here, not by external reference —
  both transports emit exactly these fields (JSON on the slog path,
  equivalent attributes on the OTLP path):

  ```json
  { "event":"biz.outcome", "biz.flow":"invoice.pay", "biz.stage":"capture",
    "biz.outcome":"failed", "biz.entity.id":"inv_8Ka…", "biz.customer.id":"h:3f9…",
    "biz.amount.minor":14900, "biz.amount.currency":"USD", "biz.amount.exponent":2,
    "biz.value.kind":"fee", "biz.amount.estimated":false, "biz.segment":"smb",
    "biz.sla.deadline":"2026-08-27T14:32:00Z", "source":"stripe:webhook",
    "error":"card_declined", "trace.id":"…" }
  ```

  **Amended 2026-08-30 (workspace-cnz).** This block previously showed
  unnamespaced names — `flow`, `entity_id`, `amount_minor`, `kind`, `est`,
  `deadline`, `trace_id` — which no exporter has ever emitted, while
  `docs/semconv.md` gave a third spelling again (`biz.entity_id`,
  `biz.money.amount_minor`, `biz.kind`, `biz.estimated`). The shipped
  spelling above is now the only one, and it is defined once in code as the
  `biz.Attr*` constants rather than repeated as literals in each exporter.
  The bare names were also a collision hazard: an event sink shared with
  anything else has no reason to reserve `flow` or `kind`.

  **Amended 2026-08-30 (workspace-8yq).** The four money attributes were
  renamed into a consistent dotted namespace: `biz.amount_minor` →
  `biz.amount.minor`, `biz.currency` → `biz.amount.currency`,
  `biz.exponent` → `biz.amount.exponent`, `biz.amount.est` →
  `biz.amount.estimated`. The workspace-cnz settlement froze the shipped
  spelling verbatim on purpose — a rename inside the settlement would have
  broken its no-bytes-moved argument — and left the inconsistency (an
  `amount.` namespace that `biz.amount_minor` did not use) as a recorded
  founder call. The founder took the break pre-1.0, when it costs a
  regenerated vector and this note instead of a major version. The block
  above shows the renamed form.

  The fallback path is a `log/slog` handler emitting this shape for shops
  without OTLP; any log pipeline that can ship JSON lines becomes an event
  sink.

  `testkit/vectors/outcome-event.json` records the field set for a fully
  populated Outcome and for one carrying only the required facts, including
  which names must be **absent** rather than empty, and
  `testkit.CheckOutcomeEvent` drives every exporter through it.

  **Amended 2026-08-30 (workspace-cnz).** That test is new. This ADR
  previously asserted "a conformance test in `testkit` asserts both
  transports produce the same fields from the same Outcome" as though it
  existed. It did not — the exporter conformance suite checked delivery
  counts and capability honesty, never field names — and the transports did
  not in fact agree: `biz.sla.deadline` reached the OTLP event alone, so an
  operator's event contents depended on which exporter they had wired. The
  CloudWatch and GCP exporters now emit it too, and the vector is what keeps
  the claim true instead of aspirational.

  The one declared difference is the trace id: OTLP carries it as the log
  record's span context, which is what a trace-aware transport should do,
  so it is absent from that transport's attributes by design rather than
  missing.
- **Export failure semantics** (review charter item 6 made contract):
  `Record` never blocks the business request path. Events buffer in a
  bounded in-memory queue (default 10k, configurable); on validation
  failure, overflow, or terminal export failure the event
  is **dropped and `biz_dropped_events_total{reason}` increments**
  (`reason` ∈ `invalid`, `overflow`, `export`). *(Amended
  2026-08-27 under ADR-0008: `invalid` added; the enum previously read
  `overflow`, `encode`, `export` — the emit contract already said invalid
  outcomes drop and count, and the enum had drifted from it. Amended
  2026-08-30 under ADR-0008, workspace-m7l: `encode` removed; `emit`
  buffers `biz.Outcome` values and never serializes them — encoding is a
  transport concern inside each adapter, so a marshal failure surfaces as
  an `ExportEvents` error and is counted as `export`. No code path has
  ever incremented `reason="encode"`.)* A silent drop is a defect; a visible drop is a
  coverage-ratio conversation. The nightly reconciliation leg exists to
  catch exactly the residue this counter admits to.
- `emit` speaks only the `Exporter` interface; transport choice is
  configuration, not code.

## Consequences

- Core stays zero-heavy-dep; the sdk/log version risk is priced in and
  fenced.
- Re-check sdk/log stability when bumping otel dependencies; move this ADR
  to "accepted, unconditional" once sdk/log tags v1.0.
- The event JSON shape is part of the public contract and versions with the
  library, independent of transport.
