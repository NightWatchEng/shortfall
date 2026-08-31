// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package biz

import (
	"fmt"
	"strings"
)

// Money is an amount in minor units (cents for USD, yen for JPY) with its
// ISO 4217 currency and the currency's decimal exponent. Never a float —
// floats drift, and drift is what ledger reconciliation exists to catch
// (ADR-0001).
type Money struct {
	Amount   int64  // minor units: 14900 = $149.00 when Exponent == 2
	Currency string // ISO 4217 alphabetic code
	Exponent int8   // decimal places of the minor unit (0..4)
}

// Validate rejects malformed money. Negative amounts are rejected in
// v0.x: outcomes carry transaction value, and refunds/adjustments are a
// modeling decision for a future ADR, not an accidental sign.
func (m Money) Validate() error {
	if m.Amount < 0 {
		return fmt.Errorf("biz: money amount %d is negative", m.Amount)
	}

	if len(m.Currency) != 3 ||
		m.Currency != strings.ToUpper(m.Currency) ||
		strings.ContainsFunc(m.Currency, func(r rune) bool { return r < 'A' || r > 'Z' }) {
		return fmt.Errorf("biz: currency %q is not an ISO 4217 alphabetic code", m.Currency)
	}

	if m.Exponent < 0 || m.Exponent > 4 {
		return fmt.Errorf("biz: exponent %d outside [0, 4]", m.Exponent)
	}

	return nil
}

// String renders the amount in major units for humans: "USD 149.00",
// "JPY 14900". Pure integer formatting — no float ever touches it. Total
// on any receiver: an invalid Money renders in a marked raw form instead
// of panicking or printing garbage.
func (m Money) String() string {
	if m.Validate() != nil {
		return fmt.Sprintf("%s INVALID(%d e%d)", m.Currency, m.Amount, m.Exponent)
	}

	if m.Exponent == 0 {
		return fmt.Sprintf("%s %d", m.Currency, m.Amount)
	}

	pow := int64(1)
	for i := int8(0); i < m.Exponent; i++ {
		pow *= 10
	}

	return fmt.Sprintf("%s %d.%0*d", m.Currency, m.Amount/pow, m.Exponent, m.Amount%pow)
}

// ParseMinor converts a decimal string ("149.00", "149.5", "149") into
// minor units for the given exponent, without floats. Excess precision is
// an error, never a silent rounding: "149.005" at exponent 2 is a caller
// bug, and money bugs must be loud.
func ParseMinor(s string, exponent int8) (int64, error) {
	if exponent < 0 || exponent > 4 {
		return 0, fmt.Errorf("biz: exponent %d outside [0, 4]", exponent)
	}

	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") {
		return 0, fmt.Errorf("biz: %q is not a decimal amount", s)
	}

	if hasDot && exponent == 0 {
		return 0, fmt.Errorf("biz: %q has decimals but the currency exponent is 0", s)
	}

	if hasDot && len(frac) > int(exponent) {
		return 0, fmt.Errorf("biz: %q has more than %d decimal places — refusing to round money silently", s, exponent)
	}

	digits := func(str string) (int64, error) {
		var v int64
		for _, r := range str {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("biz: %q is not a decimal amount", s)
			}

			d := int64(r - '0')
			if v > (1<<63-1-d)/10 {
				return 0, fmt.Errorf("biz: %q overflows int64 minor units", s)
			}

			v = v*10 + d
		}

		return v, nil
	}
	w, err := digits(whole)
	if err != nil {
		return 0, err
	}

	f := int64(0)
	if hasDot {
		if f, err = digits(frac); err != nil {
			return 0, err
		}
	}

	for i := 0; i < int(exponent)-len(frac); i++ {
		f *= 10
	}

	pow := int64(1)
	for i := int8(0); i < exponent; i++ {
		pow *= 10
	}

	if w > ((1<<63-1)-f)/pow {
		return 0, fmt.Errorf("biz: %q overflows int64 minor units", s)
	}

	return w*pow + f, nil
}
