# ADR-0016 — Value-stage anchoring: success value is read at one declared stage (ADR-0011 amendment)

Status: accepted (ADR-0011 amendment, 2026-08-28)
Date: 2026-08-28

## Context

The emitter records every stage transition (docs/inhouse.md; `emit.Std.Record`),
so a settled transaction ships success `biz_value_total` and `biz_txn_total`
points at each of its stages. Two engine readers summed success series with no
stage filter:

- Coverage's telemetry read summed success value across stages, so telemetry
  read 2–3x the ledger, the ratio clamped to 1, and the reconcile trust line
  could report 100% while the exporter dropped up to ~2/3 of captured value —
  masking exactly the failure Coverage exists to detect. Reachable via
  `shortfall reconcile --prometheus` for any per-stage-instrumented flow.
- The AOV metric fallback divided value by a cross-stage count; entry-stage
  counts with no companion value (the counterfactual entry basis) silently
  halved it.

## Decision

- Each flow has a **value stage**: the stage whose success observations carry
  the flow's value once per transaction. The registry declares it as optional
  `reconcile.stage` (validated against the declared stages); undeclared, it
  defaults to the flow's **last stage**.
- **Coverage** reads telemetry success value at the value stage only. A flow
  the registry does not know — or a nil registry — falls back to the
  unfiltered read (the pre-amendment behavior).
- The **AOV** fallback reads both the value and count sums, and the events
  query, at the value stage.
- The entries basis for the counterfactual leg is unchanged: `biz_txn_total`
  at `Stages[0]`, summed over outcomes.

## Consequences

- A dropped exporter shows up in the coverage ratio again for per-stage
  emitters; the multiply-count clamp is gone. Terminal-only emitters (the
  testkit's model) read identically to before, since only the last stage
  carries their success value.
- Registry authors whose ledger books at a non-final stage (e.g. capture with
  a later settle stage) declare `reconcile: { stage: capture }`; the default
  suits ledgers reconciled at the flow's end.
- One residual honesty note: value observed at the value stage excludes
  transactions still in flight before it — a transient undercount during
  incidents, which is the correct direction for a trust line (never
  overstating coverage).
