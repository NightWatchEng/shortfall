package biz

import (
	"fmt"
	"regexp"
)

// CheckPII rejects the three shapes that must never enter biz.*
// attributes or event fields: card numbers (PAN), email addresses, and
// IBANs. Exported so the emit and registry layers can guard free-text
// surfaces (error strings, attribute values) with the same net —
// reimplementing a PII guard is how coverage gaps are born.
//
// Documented tradeoffs, chosen deliberately:
//   - PAN detection Luhn-checks digit runs AND their dash/space-delimited
//     sub-segments (so "ord-9-<PAN>" cannot hide a card behind one stray
//     digit). A Luhn-INVALID run passes: it is an order id, not a card —
//     a guard that cries wolf gets disabled, and a disabled guard
//     protects nothing.
//   - ~10% of RANDOM numeric 13-19 digit ids are Luhn-valid by chance
//     (measured: unix-nano and unix-milli ids flag at almost exactly
//     10%). Callers using bare numeric ids in EntityID/CustomerID will
//     see data-dependent rejections: PREFIX numeric ids ("inv_170266...")
//     and the guard never fires on them.
//   - IBAN detection is case-insensitive, needs no word boundary, and
//     requires the ISO 13616 mod-97 checksum to pass, so a random
//     id matching the shape survives ~96/97 of the time.
func CheckPII(field, s string) error {
	if hasPAN(s) {
		return fmt.Errorf("biz: %s contains what looks like a card number (Luhn-valid 13-19 digit run) — PAN must never enter biz.* attributes", field)
	}
	if emailRe.MatchString(s) {
		return fmt.Errorf("biz: %s contains an email address — PII must never enter biz.* attributes", field)
	}
	if hasIBAN(s) {
		return fmt.Errorf("biz: %s contains what looks like an IBAN (mod-97 checksum passes) — PII must never enter biz.* attributes", field)
	}
	return nil
}

// rejectPII keeps the internal call sites terse.
func rejectPII(field, s string) error { return CheckPII(field, s) }

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// Shape only — the mod-97 checksum is the discriminator, so no word
	// boundary is needed and an abutting letter cannot hide a real IBAN.
	ibanRe = regexp.MustCompile(`(?i)[a-z]{2}[0-9]{2}[a-z0-9]{11,30}`)
)

// hasPAN scans for runs of digits (optionally separated by single spaces
// or dashes, as cards are commonly written). A run is flagged when the
// FULL run or any contiguous span of its separator-delimited segments
// totals 13-19 digits and passes Luhn. Checking spans matters in both
// directions: a 16-digit Luhn-valid substring of one unbroken 20-digit id
// must NOT fire (segments are only split at separators), while a PAN
// dash-joined to a stray digit must still be caught.
func hasPAN(s string) bool {
	i := 0
	for i < len(s) {
		if !isDigit(s[i]) {
			i++
			continue
		}
		digits := make([]byte, 0, 40)
		bounds := make([][2]int, 0, 8) // segment [start, end) into digits
		segStart := 0
		j := i
		for j < len(s) {
			if isDigit(s[j]) {
				digits = append(digits, s[j])
				j++
				continue
			}
			if (s[j] == ' ' || s[j] == '-') && j+1 < len(s) && isDigit(s[j+1]) {
				bounds = append(bounds, [2]int{segStart, len(digits)})
				segStart = len(digits)
				j++
				continue
			}
			break
		}
		bounds = append(bounds, [2]int{segStart, len(digits)})

		// Every contiguous span of whole segments, PAN-length, Luhn-checked.
		for a := 0; a < len(bounds); a++ {
			for b := a; b < len(bounds); b++ {
				span := digits[bounds[a][0]:bounds[b][1]]
				if n := len(span); n >= 13 && n <= 19 && luhnValid(span) {
					return true
				}
			}
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

// hasIBAN finds shape candidates case-insensitively and accepts only
// those whose ISO 13616 mod-97 checksum equals 1.
func hasIBAN(s string) bool {
	for _, cand := range ibanRe.FindAllString(s, -1) {
		if ibanMod97(cand) == 1 {
			return true
		}
	}
	return false
}

// ibanMod97 computes the ISO 13616 checksum: move the first four
// characters to the end, map letters to 10..35, take the whole decimal
// string mod 97 — incrementally, so no big integers are needed.
func ibanMod97(iban string) int {
	rem := 0
	feed := func(c byte) bool {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			v := int(c-'A') + 10
			rem = (rem*100 + v) % 97
		case c >= 'a' && c <= 'z':
			v := int(c-'a') + 10
			rem = (rem*100 + v) % 97
		default:
			return false
		}
		return true
	}
	for i := 4; i < len(iban); i++ {
		if !feed(iban[i]) {
			return -1
		}
	}
	for i := 0; i < 4; i++ {
		if !feed(iban[i]) {
			return -1
		}
	}
	return rem
}
