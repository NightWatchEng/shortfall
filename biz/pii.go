package biz

import (
	"fmt"
	"regexp"
)

// The PII guard rejects the three shapes that must never enter biz.*
// attributes: card numbers (PAN), email addresses, and IBANs. It is
// deliberately conservative in what it flags — a Luhn-INVALID digit run
// is an order id, not a card — because a guard that cries wolf gets
// disabled, and a disabled guard protects nothing.

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	ibanRe  = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}\b`)
)

func rejectPII(field, s string) error {
	if hasPAN(s) {
		return fmt.Errorf("biz: %s contains what looks like a card number (Luhn-valid 13-19 digit run) — PAN must never enter biz.* attributes", field)
	}
	if emailRe.MatchString(s) {
		return fmt.Errorf("biz: %s contains an email address — PII must never enter biz.* attributes", field)
	}
	if ibanRe.MatchString(s) {
		return fmt.Errorf("biz: %s contains what looks like an IBAN — PII must never enter biz.* attributes", field)
	}
	return nil
}

// hasPAN scans for MAXIMAL runs of digits (optionally separated by single
// spaces or dashes, as cards are commonly written) totalling 13-19 digits
// that pass the Luhn check. Maximal-run extraction matters: a 20-digit id
// must not be flagged because a 16-digit substring of it happens to pass
// Luhn — regex substring matching gets exactly that wrong.
func hasPAN(s string) bool {
	i := 0
	for i < len(s) {
		if !isDigit(s[i]) {
			i++
			continue
		}
		// Extend the maximal run: digits, with single separators allowed
		// only between digits.
		var digits []byte
		j := i
		for j < len(s) {
			if isDigit(s[j]) {
				digits = append(digits, s[j])
				j++
				continue
			}
			if (s[j] == ' ' || s[j] == '-') && j+1 < len(s) && isDigit(s[j+1]) {
				j++
				continue
			}
			break
		}
		if n := len(digits); n >= 13 && n <= 19 && luhnValid(digits) {
			return true
		}
		i = j
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// luhnValid implements the Luhn checksum over a digit slice.
func luhnValid(digits []byte) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
