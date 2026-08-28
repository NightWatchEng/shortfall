# Business impact — invoice.pay

Window `2026-08-27T14:00Z → 2026-08-27T16:00Z` · registry v1 · v0.1.0

_Amounts are minor currency units (ADR-0001). Realized value and estimated value are reported separately and never summed._

| Leg | Evidence | Value |
|---|---|---|
| Realized | deterministic | USD 15000 |
| Deferred (in-flight) | deterministic | USD 5568661 |
| Deferred → projected lost | deterministic | USD 500 |
| Unrealized (counterfactual) | estimate | USD 2000 … USD 8000 (mid USD 5000) |

> Unrealized is an estimate range and must not be added to realized.

## Customers

2 distinct affected (enterprise 1, smb 1).

| Customer | Segment | Failed value |
|---|---|---|
| h:c2 | enterprise | USD 9000 |
| h:c1 | smb | USD 6000 |

## Coverage

98.0% of telemetry reconciled against `sql:ledger.payments` [trust].

**Suggested severity:** SEV2
