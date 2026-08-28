package biz

import "fmt"

// LedgerRow is one reconciliation fact from a payment provider's own books: the
// ground-truth money that moved for a (Flow, currency, Outcome) slice. The
// coverage leg (M8) compares these sums against the telemetry sums for the same
// slice — the degree to which they agree is the Finance-trust number. Amounts
// are minor units and are never summed across currencies (ADR-0001), so the
// currency is part of the row's identity, carried inside Money.
type LedgerRow struct {
	Flow    string // biz flow, from provider metadata; "" when the record was unattributed
	Outcome Result // success / failed / deferred — the terminal fact the provider recorded
	Money   Money  // Money.Amount is the SUM over the slice; Currency/Exponent identify it
	Count   int64  // number of provider records in the slice
}

// Validate rejects a malformed row. A row must carry a valid outcome and valid
// money, and count and summed amount must be consistent (a positive sum with a
// zero count, or vice versa, is a reconciliation bug, not a ledger fact).
func (r LedgerRow) Validate() error {
	switch r.Outcome {
	case ResultSuccess, ResultFailed, ResultDeferred:
	default:
		return fmt.Errorf("biz: ledger row outcome %q is not success/failed/deferred", r.Outcome)
	}
	if err := r.Money.Validate(); err != nil {
		return fmt.Errorf("biz: ledger row money: %w", err)
	}
	if r.Count < 0 {
		return fmt.Errorf("biz: ledger row count %d is negative", r.Count)
	}
	if r.Count == 0 && r.Money.Amount != 0 {
		return fmt.Errorf("biz: ledger row has summed amount %d over zero records", r.Money.Amount)
	}
	return nil
}
