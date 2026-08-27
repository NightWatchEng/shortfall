# ADR-0006 — Baseline v0: hour-of-week robust median; ML only behind the interface

Status: proposed (ratify at the M2 interface freeze)
Date: 2026-08-27

## Context

The unrealized leg needs an expected-traffic series with error bars. The
audience is Finance in an incident channel: a number they cannot have
explained to them in one sentence is a number they will not sign. Traffic
in payment flows is dominated by hour-of-week seasonality; holidays are the
known exception. Sophistication beyond that (Prophet, gradient boosting)
buys accuracy the pilot does not need at the price of explainability it
cannot afford.

## Decision

- **Expected count** for a given hour = robust median of the same
  hour-of-week over the registry's lookback (default 8 weeks), with
  holidays excluded per the registry's holiday calendar.
- **Interval** = expected ± **2 × 1.4826 × MAD** (median absolute
  deviation of the same hour-of-week samples; 1.4826 is the normal
  consistency factor, 2 gives an ≈95% band under normality). Both constants
  are part of the contract — two conforming implementations must produce
  the same range. Reported always; the unrealized leg is a range or it is
  nothing.
- **Recovery** = the registry's `recovered_fraction` applied linearly
  within the `within` window (usage-loss-curve prior art, telecom).
- The whole thing sits behind a `Baseline` interface. A smarter
  implementation (seasonal decomposition, Prophet sidecar) is a new
  implementation of that interface, opt-in per flow via the registry —
  never a silent upgrade of the default.
- One sentence for Finance, recorded here: "expected volume is the median
  of the same hour over the last N non-holiday weeks; the ± is how much
  that hour normally varies."

## Consequences

- Deterministic given data and registry: two invocations agree, which
  matters when the number lands in a postmortem.
- Known blind spots, accepted for v0: trend growth within the lookback,
  holiday-of-year effects, and flows younger than the lookback (the report
  flags a retention gap rather than degrading silently).
- Accuracy target, self-contained here: **median absolute percentage error
  < 5%** of hourly expected vs actual stage-entry counts, computed over all
  non-incident, non-holiday hours of a 4-week synthetic evaluation window
  on the harness's known traffic curve — measured in M7 before the leg
  ships.
