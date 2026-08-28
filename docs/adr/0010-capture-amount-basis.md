# ADR-0010 — Capture amount basis: intended `amount`, not `amount_received` (v0)

Status: accepted (2026-08-28)
Date: 2026-08-28

## Context

A payment provider reports two amounts for a captured transaction: the
**intended** amount (what the charge was for) and the **captured/received**
amount (what actually settled). They differ on a **partial capture** — a
merchant captures less than authorized (a partial refund at capture, a
split shipment, an adjusted invoice).

Two shortfall paths read a provider's captured successes:

- the **inbound webhook** (`adapters/payment/stripe`, ADR-driven), which maps
  `payment_intent.succeeded` to a realized capture. Its payload struct reads
  the event's `amount` field (there is no `amount_received` field on it).
- the **outbound reconciler** (`stripe.Reconcile`, workspace-tmw.6.3), which
  pages payment intents into `biz.LedgerRow`s the coverage leg compares
  against telemetry.

The coverage ratio (M8) is only meaningful if **both sides measure the same
quantity**. If the webhook records intended `amount` while the reconciler
records `amount_received`, every partial capture reads as a false coverage
shortfall — telemetry and the ledger disagree for a non-drift reason, which is
the opposite of what coverage exists to detect.

## Decision

For v0, the canonical basis for a captured success is the **intended
`amount`**, on **both** the webhook and the reconciler paths. This is already
how both are implemented; this ADR ratifies it so the choice is explicit and
reviewed rather than incidental.

- Coverage therefore measures **event drift** (did telemetry see the same
  transactions the provider's books did), not capture policy.
- Genuine partial-capture shortfall (intended minus captured, real money that
  authorized but did not settle) is **not measured** by any leg in v0. It is a
  distinct quantity from incident-driven loss and is deliberately out of scope
  for the pilot.

## Consequences

- A reconciled ledger and the telemetry it is checked against agree to 100% on
  a healthy window even when partial captures occur — the coverage number stays
  trustworthy.
- The library under-reports the specific dollars lost to partial captures.
  Accepted for v0; surfacing it is a future, separate leg (or a coverage
  sub-line), and would require:
  - the webhook payload struct to also read `amount_received`;
  - the reconciler to carry both amounts on `biz.LedgerRow` (or a second row
    kind);
  - a decision on whether partial-capture shortfall is "loss" for the incident
    report or a standalone finance metric.
- Changing the basis to `amount_received` later is an ADR revision that MUST
  move both paths together — a split basis is the one outcome this ADR forbids.
