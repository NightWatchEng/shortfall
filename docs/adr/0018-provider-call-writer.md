# ADR-0018 — biz_provider_calls_total gets a writer, and its cardinality fence moves into the library

Status: accepted (ADR-0004 metric-family amendment, 2026-08-30)
Date: 2026-08-30

## Context

ADR-0004 declared `biz_provider_calls_total{provider, op, outcome}` and all
four export adapters map it. `engine.Compute` reads
`biz_provider_calls_total{outcome=failed}` over the window and appends an
upstream-attribution hint when it is non-zero — the unrealized leg's only
signal for telling a provider outage from internal suppression.

Nothing in the library could increment it. `emit.Emitter` exposed `Record`
and `SetInFlight`; `adapters/payment/stripe` hands every observed API call
to a `WithProviderMetric` callback whose doc said to wire it to the family,
but the only way to do that was to construct an `emit.MetricPoint` by hand
and call `Exporter.ExportMetrics` directly — bypassing the event buffer, the
ADR-0004 label fence and `biz_dropped_events_total`. So the attribution hint
was dead code for every user who followed the documented path, and the one
available workaround was the exact cardinality hole ADR-0004 exists to close
(workspace-cp2).

ADR-0004 justified `provider` and `op` as "bounded by construction (a handful
of payment providers, each with a small API surface); they are
adapter-supplied constants, never request data". That is a statement about
caller discipline. ADR-0004's own decision is that cardinality "must be
enforced by the emitting code itself" and made "structurally impossible
rather than discouraged" — a standard the family did not meet, because no
emitting code existed for it.

## Decision

`emit.Emitter` gains a third write path:

`RecordProviderCall(provider, op, outcome string)` — one counted observation
of a downstream provider call on `biz_provider_calls_total{provider, op,
outcome}`, routed through the same bounded metric buffer, label fence and
drop counter as the other families.

- **`outcome` is a closed two-value enum**, `success` and `failed`, exported
  as `emit.ProviderCallOutcomes`. This family describes the provider, not a
  transaction: `success` means the provider returned a definitive answer (the
  API was reachable) and `failed` means it did not (transport error, timeout,
  5xx, 429). A declined card is the provider answering correctly — a success
  here and a `failed` outcome *event* on the stage, never both on this family.
  The wider five-value `biz.Result` enumeration stays on the transaction
  families. Anything else is dropped and counted.
- **`provider` and `op` must look like the constants ADR-0004 assumes**:
  non-blank, at most 64 bytes, valid UTF-8, no control characters. Anything
  else is dropped and counted on `biz_dropped_events_total{reason=invalid}`,
  never silently. These are the only metric labels built from a raw caller
  string — every other label axis is registry-validated, enum-fenced, or
  validated `biz.Money` — so this is the one place invalid UTF-8 could reach
  a label, and it must not: the OTLP exporter marshals label values as
  protobuf strings, which fails the entire batch on a bad byte, and a failed
  metric batch is dropped whole.
- **The bound becomes structural.** An emitter admits at most
  `emit.DefaultProviderPairCap` (64) distinct (`provider`, `op`) pairs; every
  unseen pair past that collapses to the fixed value `emit.ProviderOther`
  (`other`). The call is still counted, so sums stay complete, and pairs
  admitted before the cap keep their own labels afterwards. This mirrors the
  registry fence's `unregistered` fallback — the label is replaced, the
  observation is still counted. It deliberately does NOT follow
  `InFlightTracker`'s max-items bound, which drops the `Track` call and
  reports the shortfall through `Overflowed()`: that bound protects memory
  and accepts a published understatement, while this one protects
  cardinality and must not lose counts, because the engine reads this family
  as a presence signal. A caller that hands us request data inflates one
  series instead of the whole family.

This amends the second bullet of ADR-0004 §"Decision" (the one beginning
"`provider` and `op` on `biz_provider_calls_total` are bounded by
construction"). This ADR mandates that bullet be annotated in place with a
dated amendment note, per the CONTRIBUTING carve-out for a reconciliation an
incoming ADR explicitly requires.

## Consequences

- `emit.Emitter` is amended. It is the frozen v0.1.0 contract, so this breaks
  every out-of-tree implementation at compile time — loudly, at build, never
  silently at runtime. In-tree there are two implementations (`Std` and the
  benchmark's `countingEmitter`), the same "the only implementation is `Std`"
  position ADR-0012 amended from. The repo is pre-1.0; the same amendment
  after v1.0.0 would cost a major version.
- The engine's upstream-attribution hint is reachable from the documented
  path for the first time. `adapters/payment/stripe`'s `WithProviderMetric`
  callback wires to it in one line, shipped as a runnable example
  (`ExampleEmitter_RecordProviderCall`).
- A port now has a reference writer to conform to, not just a family name.
  `docs/portability.md` states the cap as a MUST-have-one, MAY-differ: a
  conforming producer must bound the pair count, but may pick its own number.
- ADR-0004's second Decision bullet carries a dated amendment note recording
  what it said before and what changed, as this ADR mandates above. ADR-0004
  is not otherwise rewritten — its history stands.
- No change to the family name, its ADR-0004 label set, the four export
  adapter mappings, or `engine`'s read path.
- The exporter conformance suite seeds this family (`testkit/conformance`,
  case 3 of the sample interleave, alongside `biz_txn_total` and
  `biz_dropped_events_total`). **Amended 2026-08-30 (workspace-v42)**: at
  acceptance this bullet said the suite "still never seeds this family" and
  that the gap was tracked separately — workspace-crg closed that gap, and
  the bullet described the pre-crg world until this amendment.
