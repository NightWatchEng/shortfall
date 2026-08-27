# Architecture decision records

One ADR per irreversible design decision. Status moves proposed → accepted
at the M2 interface freeze; superseding requires a new ADR, never an edit
that rewrites history.

| # | Decision | Status |
|---|---|---|
| [0001](0001-money-representation.md) | Money is int64 minor units + currency + exponent, never float | proposed |
| [0002](0002-outcome-event-transport.md) | Outcome events ride the OTel Log SDK (isolated in the OTLP adapter), slog fallback | proposed |
| [0003](0003-value-context-propagation.md) | ValueContext propagates as one versioned Baggage member, ≤512 bytes | proposed |
| [0004](0004-metric-label-set.md) | Exactly six metric labels; cardinality is a library guarantee | proposed |
| [0005](0005-inflight-age-buckets.md) | Fixed in-flight age buckets (lt1m … gt2h) | proposed |
| [0006](0006-baseline-v0.md) | Baseline v0: hour-of-week robust median + MAD interval; ML only behind the interface | proposed |
