# Architecture decision records

One ADR per irreversible design decision. Status moves proposed → accepted
at the M2 interface freeze; superseding requires a new ADR, never an edit
that rewrites history.

| # | Decision | Status |
|---|---|---|
| [0001](0001-money-representation.md) | Money is int64 minor units + currency + exponent, never float | accepted |
| [0002](0002-outcome-event-transport.md) | Outcome events ride the OTel Log SDK (isolated in the OTLP adapter), slog fallback | accepted |
| [0003](0003-value-context-propagation.md) | ValueContext propagates as one versioned Baggage member, ≤512 bytes | accepted |
| [0004](0004-metric-label-set.md) | Exactly six metric labels; cardinality is a library guarantee | accepted |
| [0005](0005-inflight-age-buckets.md) | Fixed in-flight age buckets (lt1m … gt2h) | accepted |
| [0006](0006-baseline-v0.md) | Baseline v0: hour-of-week robust median + MAD interval; ML only behind the interface | accepted |
| [0007](0007-table-driven-tests.md) | Table-driven tests (named cases + t.Run) are the repo standard; declared exceptions only | accepted |
| [0008](0008-docs-tell-the-truth.md) | Documentation tells the truth: honest tense, enforced-means-enforced, same-PR updates | accepted |
