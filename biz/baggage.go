package biz

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/baggage"
)

// The ValueContext wire codec (ADR-0003): one versioned Baggage member,
// biz.vc, so async carriers copy exactly one header. The encoded value is
// capped at MaxEncodedBytes, measured as UTF-8 bytes before any
// percent-encoding the Baggage wire layer adds, and every byte emitted is
// inside the W3C baggage value grammar (no spaces, quotes, commas,
// semicolons, or backslashes survive escaping).
//
// DecodeVC returns exactly what the wire carried; it does not run
// Validate — transport fidelity and semantic validity are separate
// judgments, and the emit layer makes the second one.

// MemberKey is the single Baggage member the whole context rides in.
const MemberKey = "biz.vc"

// MaxEncodedBytes caps the encoded member value (ADR-0003).
const MaxEncodedBytes = 512

// codecVersion leads every encoding; decoders reject versions they do
// not know rather than guessing.
const codecVersion = "1"

// Deadline domain on the wire: strictly after the epoch and no later
// than year 3000, carried at unix-second precision (sub-second components
// are discarded at encode). Both codec directions enforce the same
// bounds, so everything encoded decodes and a peer-controlled header can
// never smuggle a time.Time-overflowing instant into SLA math.
const maxDeadlineUnix = 32503680000 // 3000-01-01T00:00:00Z

// OversizeError reports an encoding that exceeded MaxEncodedBytes.
type OversizeError struct {
	Size int
}

func (e *OversizeError) Error() string {
	return fmt.Sprintf("biz: encoded ValueContext is %d bytes, cap is %d (ADR-0003)", e.Size, MaxEncodedBytes)
}

func asOversize(err error, target **OversizeError) bool { return errors.As(err, target) }

// EncodeVC encodes a ValueContext into the biz.vc wire form:
//
//	1|flow|entity|customer|segment|amount|currency|exponent|kind|flags|deadlineUnix
//
// Every string field is escaped uniformly, so no field can smuggle a
// delimiter and re-encoding a decoded context is always byte-stable.
func EncodeVC(vc ValueContext) (string, error) {
	deadline := int64(0)
	if !vc.Deadline.IsZero() {
		deadline = vc.Deadline.Unix()
		// Zero on the wire means "no deadline", so the encodable domain excludes
		// it and everything the decoder rejects — else context drops on the next hop.
		if deadline <= 0 || deadline > maxDeadlineUnix {
			return "", fmt.Errorf("biz: deadline %v outside the encodable domain (1970, 3000]", vc.Deadline)
		}
	}
	flags := int64(0)
	if vc.Estimated {
		flags = 1
	}
	var b strings.Builder
	b.Grow(96)
	b.WriteString(codecVersion)
	for _, f := range []string{vc.Flow, vc.EntityID, vc.CustomerID, vc.Segment} {
		b.WriteByte('|')
		escapeInto(&b, f)
	}
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(vc.Money.Amount, 10))
	b.WriteByte('|')
	escapeInto(&b, vc.Money.Currency)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(int64(vc.Money.Exponent), 10))
	b.WriteByte('|')
	escapeInto(&b, string(vc.Kind))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(flags, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(deadline, 10))

	s := b.String()
	if len(s) > MaxEncodedBytes {
		return "", &OversizeError{Size: len(s)}
	}
	return s, nil
}

// DecodeVC decodes the biz.vc wire form. Unknown versions, wrong field
// counts, malformed escapes, and malformed numbers are errors — a codec
// that guesses is a codec that corrupts money context silently.
func DecodeVC(s string) (ValueContext, error) {
	var vc ValueContext
	if len(s) > MaxEncodedBytes {
		return vc, fmt.Errorf("biz: biz.vc value %d bytes exceeds the %d cap", len(s), MaxEncodedBytes)
	}
	fields := strings.Split(s, "|")
	if len(fields) != 11 {
		return vc, fmt.Errorf("biz: biz.vc has %d fields, want 11", len(fields))
	}
	if fields[0] != codecVersion {
		return vc, fmt.Errorf("biz: unknown biz.vc version %q", fields[0])
	}
	var err error
	if vc.Flow, err = unescape(fields[1]); err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc flow: %w", err)
	}
	if vc.EntityID, err = unescape(fields[2]); err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc entity: %w", err)
	}
	if vc.CustomerID, err = unescape(fields[3]); err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc customer: %w", err)
	}
	if vc.Segment, err = unescape(fields[4]); err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc segment: %w", err)
	}
	if vc.Money.Amount, err = strconv.ParseInt(fields[5], 10, 64); err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc amount %q", fields[5])
	}
	if vc.Money.Currency, err = unescape(fields[6]); err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc currency: %w", err)
	}
	exp, err := strconv.ParseInt(fields[7], 10, 8)
	if err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc exponent %q", fields[7])
	}
	vc.Money.Exponent = int8(exp)
	kind, err := unescape(fields[8])
	if err != nil {
		return ValueContext{}, fmt.Errorf("biz: biz.vc kind: %w", err)
	}
	vc.Kind = Kind(kind)
	switch fields[9] {
	case "0":
	case "1":
		vc.Estimated = true
	default:
		return ValueContext{}, fmt.Errorf("biz: biz.vc flags %q not defined in version 1", fields[9])
	}
	deadline, err := strconv.ParseInt(fields[10], 10, 64)
	if err != nil || deadline < 0 || deadline > maxDeadlineUnix {
		return ValueContext{}, fmt.Errorf("biz: biz.vc deadline %q outside [0, %d]", fields[10], int64(maxDeadlineUnix))
	}
	if deadline > 0 {
		vc.Deadline = time.Unix(deadline, 0).UTC()
	}
	return vc, nil
}

// WithValueContext encodes vc into the biz.vc Baggage member on ctx.
// One member, one header for the queue carriers to copy (ADR-0003).
func WithValueContext(ctx context.Context, vc ValueContext) (context.Context, error) {
	enc, err := EncodeVC(vc)
	if err != nil {
		return ctx, err
	}
	member, err := baggage.NewMemberRaw(MemberKey, enc)
	if err != nil {
		return ctx, fmt.Errorf("biz: baggage member: %w", err)
	}
	bag, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		return ctx, fmt.Errorf("biz: baggage set: %w", err)
	}
	return baggage.ContextWithBaggage(ctx, bag), nil
}

// FromContext decodes the biz.vc member. The three outcomes are
// distinguishable on purpose — a corrupted header must never be mistaken
// for an absent one:
//
//	(vc, true, nil)    present and well-formed
//	(_, false, nil)    absent
//	(_, false, err)    present but corrupt — surface this into a counter
func FromContext(ctx context.Context) (ValueContext, bool, error) {
	member := baggage.FromContext(ctx).Member(MemberKey)
	if member.Key() == "" {
		return ValueContext{}, false, nil
	}
	vc, err := DecodeVC(member.Value())
	if err != nil {
		return ValueContext{}, false, err
	}
	return vc, true, nil
}

// Escaping: %XX for the delimiter, the escape byte itself, everything the
// W3C baggage value grammar forbids, and anything outside printable
// ASCII. Uniform across all string fields so re-encoding is byte-stable.
const hexDigits = "0123456789ABCDEF"

func needsEscape(c byte) bool {
	switch c {
	case '|', '%', '"', ',', ';', '\\', ' ':
		return true
	}
	return c < 0x21 || c > 0x7E
}

func escapeInto(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if needsEscape(c) {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0F])
			continue
		}
		b.WriteByte(c)
	}
}

// unescape rejects any raw byte the encoder would have escaped, so
// re-encoding a decoded input never lengthens it and the 512 cap
// survives round trips. Some never-emitted forms are still accepted —
// non-canonical %XX of bytes needing no escape, zero-padded or
// plus-signed numbers — all of which only shrink on re-encode. Escapes
// are uppercase hex only ("%7C", never "%7c"): independent
// implementations of this wire format must emit uppercase.
func unescape(s string) (string, error) {
	// Fast path: no escapes and nothing that should have been — return
	// the input without copying.
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] == '%' || needsEscape(s[i]) {
			clean = false
			break
		}
	}
	if clean {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			if needsEscape(c) {
				return "", fmt.Errorf("raw byte %q must be %%XX-escaped in canonical biz.vc", c)
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated %% escape")
		}
		hi, lo := hexVal(s[i+1]), hexVal(s[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("bad %% escape %q", s[i:min(i+3, len(s))])
		}
		b.WriteByte(byte(hi<<4 | lo))
		i += 2
	}
	return b.String(), nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
