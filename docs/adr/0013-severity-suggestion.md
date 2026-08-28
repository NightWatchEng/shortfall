# ADR-0013 — Suggested severity: a registry $/min-at-risk ladder, worst currency

Status: accepted (2026-08-28)
Date: 2026-08-28

## Context

Two incidents with the same error rate are not the same emergency: a 2% failure
on a $2M/hour flow is a page; the same rate on a $200/hour flow is not. The
report already carries a `Severity` suggestion field; it needs a rule to fill
it so responders get a consistent, registry-driven starting point rather than
eyeballing the dollar legs.

The natural input is a rate — dollars per minute at risk — but money does not
sum across currencies (ADR-0001), so a single cross-currency rate is not
well-defined, the same tension the coverage leg faced (ADR-0011).

## Decision

- The registry declares an optional **severity ladder**: an ordered list of
  `{sev, min_per_minute}` thresholds, most-severe first, with **strictly
  decreasing** floors (so "the highest sev a rate clears" is unambiguous).
  Floors are minor units per minute. The ladder is optional — no ladder means
  no suggestion, never a fabricated one.
- **At-risk** = realized loss + deferred (in-flight) value — money the incident
  has already lost or put at stake. The **rate** is at-risk ÷ window minutes.
- The rate is evaluated **per currency** (no cross-currency sum) and the
  suggestion is the **most-severe level any currency triggers** — the same
  weakest-link/worst-slice bias as coverage: a high-value flow pages even if a
  co-incident low-value flow would not.
- A rate at or above a floor triggers it (`>=`). Nothing clearing the lowest
  floor yields "" (no suggestion), as does a zero-length window.
- It is a **suggestion**, rendered as "(suggested)" — never an automatic page.
  The report states it; a human or an alerting rule decides.

## Consequences

- Deterministic and ADR-0001-safe: floors are compared to same-currency rates
  only; the headline is a max over per-currency evaluations, never a summed
  cross-currency figure.
- The floors are minor units at the registry author's currency/exponent. A
  multi-currency registry applies the same numeric floors to each currency's
  minor-unit rate — acceptable for the pilot's single-currency flows;
  per-currency ladders are a future refinement, not a v0 guarantee.
- "At risk" deliberately includes deferred value (exposure), not just realized
  loss — an incident escalating into a large in-flight backlog should raise the
  suggestion before the backlog converts to realized loss.
