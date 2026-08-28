package biz

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/baggage"
)

func codecVC() ValueContext {
	return ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_8Ka92j",
		CustomerID: "h:3f9ac2",
		Segment:    "smb",
		Money:      Money{Amount: 14900, Currency: "USD", Exponent: 2},
		Kind:       KindFee,
		Estimated:  true,
		Deadline:   time.Date(2026, 8, 27, 14, 32, 0, 0, time.UTC),
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	vc := codecVC()
	enc, err := EncodeVC(vc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasPrefix(enc, "1|") {
		t.Fatalf("encoding must lead with the version token: %q", enc)
	}
	got, err := DecodeVC(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Deadline.Equal(vc.Deadline) {
		t.Fatalf("deadline drifted: %v vs %v", got.Deadline, vc.Deadline)
	}
	got.Deadline, vc.Deadline = time.Time{}, time.Time{}
	if got != vc {
		t.Fatalf("round trip drifted:\n got %+v\nwant %+v", got, vc)
	}
}

func TestEncodeEscapesDelimiters(t *testing.T) {
	vc := codecVC()
	// Printable-ASCII ids may contain every character our format and the
	// W3C baggage value grammar care about.
	vc.EntityID = `a|b%c"d;e,f\g`
	vc.CustomerID = `%7C|%`
	enc, err := EncodeVC(vc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, forbidden := range []string{`"`, ";", ",", `\`, " "} {
		if strings.Contains(enc, forbidden) {
			t.Fatalf("encoded value contains baggage-unsafe %q: %q", forbidden, enc)
		}
	}
	got, err := DecodeVC(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EntityID != vc.EntityID || got.CustomerID != vc.CustomerID {
		t.Fatalf("escaping drifted: %+v", got)
	}
}

func TestEncodeSizeCap(t *testing.T) {
	vc := codecVC()
	vc.EntityID = strings.Repeat("x", 128)
	vc.CustomerID = strings.Repeat("y", 128)
	if _, err := EncodeVC(vc); err != nil {
		t.Fatalf("max-length ids must fit the cap: %v", err)
	}
	// Escaping inflates: 128 percent signs become 384 bytes each field.
	vc.EntityID = strings.Repeat("%", 128)
	vc.CustomerID = strings.Repeat("%", 128)
	vc.Flow = strings.Repeat("f", 64)
	_, err := EncodeVC(vc)
	var oversize *OversizeError
	if err == nil || !asOversize(err, &oversize) {
		t.Fatalf("want OversizeError, got %v", err)
	}
	if oversize.Size <= MaxEncodedBytes {
		t.Fatalf("oversize error carries size %d <= cap %d", oversize.Size, MaxEncodedBytes)
	}
}

func TestDecodeRejections(t *testing.T) {
	valid, _ := EncodeVC(codecVC())
	cases := map[string]string{
		"empty":                      "",
		"unknown version":            "2" + valid[1:],
		"field count (version gone)": strings.TrimPrefix(valid, "1|"),
		"truncated":                  valid[:len(valid)/2],
		"extra field":                valid + "|extra",
		"bad amount":                 strings.Replace(valid, "14900", "14x00", 1),
		"bad escape":                 "1|f|%zz|c|s|1|USD|2|fee|0|0",
		"lowercase hex escape":       "1|f|%7c|c|s|1|USD|2|fee|0|0",
		"raw byte needing escape":    "1|f|a b|c|s|1|USD|2|fee|0|0",
		"raw quote needing escape":   `1|f|a"b|c|s|1|USD|2|fee|0|0`,
		"negative deadline":          "1|f|e|c|s|1|USD|2|fee|0|-5",
		"deadline beyond year 3000":  "1|f|e|c|s|1|USD|2|fee|0|9223372036854775807",
		"flags outside version 1":    "1|f|e|c|s|1|USD|2|fee|2|0",
		"oversize decode":            "1|" + strings.Repeat("a", 600),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeVC(s); err == nil {
				t.Errorf("decode accepted %q", s)
			}
		})
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok, err := FromContext(ctx); ok || err != nil {
		t.Fatalf("empty context: ok=%v err=%v, want absent with no error", ok, err)
	}
	vc := codecVC()
	ctx, err := WithValueContext(ctx, vc)
	if err != nil {
		t.Fatalf("WithValueContext: %v", err)
	}
	got, ok, err := FromContext(ctx)
	if !ok || err != nil {
		t.Fatalf("ValueContext lost in context round trip: ok=%v err=%v", ok, err)
	}
	if got.EntityID != vc.EntityID || got.Money != vc.Money {
		t.Fatalf("context round trip drifted: %+v", got)
	}
	// Re-setting replaces rather than duplicates.
	ctx, err = WithValueContext(ctx, got)
	if err != nil {
		t.Fatalf("second WithValueContext: %v", err)
	}
	if _, ok, err := FromContext(ctx); !ok || err != nil {
		t.Fatalf("second write lost the member: ok=%v err=%v", ok, err)
	}
}

func TestFromContextDistinguishesCorruptFromAbsent(t *testing.T) {
	member, err := baggage.NewMemberRaw(MemberKey, "1|f|e|c|s|NOTANUMBER|USD|2|fee|0|0")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)
	_, ok, err := FromContext(ctx)
	if ok {
		t.Fatal("corrupt member decoded as ok")
	}
	if err == nil {
		t.Fatal("corrupt member indistinguishable from absent — corruption must be loud")
	}
}

func TestWithValueContextPreservesForeignMembers(t *testing.T) {
	foreign, err := baggage.NewMemberRaw("tenant", "acme")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(foreign)
	if err != nil {
		t.Fatal(err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)
	ctx, err = WithValueContext(ctx, codecVC())
	if err != nil {
		t.Fatal(err)
	}
	out := baggage.FromContext(ctx)
	if out.Member("tenant").Value() != "acme" {
		t.Fatal("foreign baggage member clobbered")
	}
	if out.Len() != 2 {
		t.Fatalf("baggage has %d members, want 2", out.Len())
	}
}

func TestDeadlineDomain(t *testing.T) {
	rejections := []struct {
		name     string
		deadline time.Time
	}{
		{"pre-1970", time.Date(1969, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"epoch is the no-deadline sentinel", time.Unix(0, 0)},
		{"beyond year 3000", time.Date(3001, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range rejections {
		t.Run(c.name, func(t *testing.T) {
			vc := codecVC()
			vc.Deadline = c.deadline
			if _, err := EncodeVC(vc); err == nil {
				t.Fatal("deadline outside the encodable domain was encoded — the decoder would reject it on the next hop")
			}
		})
	}
	vc := codecVC()
	// Sub-second precision truncates by contract: the codec carries unix
	// seconds.
	vc.Deadline = time.Date(2026, 8, 27, 14, 32, 0, 500_000_000, time.UTC)
	enc, err := EncodeVC(vc)
	if err != nil {
		t.Fatalf("sub-second deadline: %v", err)
	}
	got, err := DecodeVC(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deadline.Equal(time.Date(2026, 8, 27, 14, 32, 0, 0, time.UTC)) {
		t.Fatalf("sub-second deadline did not truncate to the second: %v", got.Deadline)
	}
}

// randomVC builds arbitrary-but-encodable ValueContexts: ids over the
// full printable-ASCII space, every kind, deadlines on and off.
func randomVC(rng *rand.Rand) ValueContext {
	printable := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('!' + rng.IntN('~'-'!'+1))
		}
		return string(b)
	}
	lower := func(n int) string {
		const alpha = "abcdefghijklmnopqrstuvwxyz0123456789._-"
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[rng.IntN(len(alpha))]
		}
		return string(b)
	}
	kinds := []Kind{KindGMV, KindNetRevenue, KindFee, KindTakeRate}
	vc := ValueContext{
		Flow:       lower(1 + rng.IntN(64)),
		EntityID:   printable(1 + rng.IntN(128)),
		CustomerID: printable(rng.IntN(129)),
		Segment:    lower(rng.IntN(33)),
		Money: Money{
			Amount:   rng.Int64N(1 << 40),
			Currency: string([]byte{byte('A' + rng.IntN(26)), byte('A' + rng.IntN(26)), byte('A' + rng.IntN(26))}),
			Exponent: int8(rng.IntN(5)),
		},
		Kind:      kinds[rng.IntN(len(kinds))],
		Estimated: rng.IntN(2) == 1,
	}
	if rng.IntN(2) == 1 {
		// Strictly inside the encodable domain (1970, 3000].
		vc.Deadline = time.Unix(1+rng.Int64N(32503679999), 0).UTC()
	}
	return vc
}

// TestRoundTripMillionIterations is the ADR-0003 acceptance bar: 1M
// seeded random round trips, byte-exact equality. Deterministic seed so a
// failure reproduces; -short runs a 50k slice for quick local loops.
// ADR-0007 exception: property loop — the case generator is the point.
func TestRoundTripMillionIterations(t *testing.T) {
	n := 1_000_000
	if testing.Short() {
		n = 50_000
	}
	rng := rand.New(rand.NewPCG(2026, 827))
	for i := 0; i < n; i++ {
		vc := randomVC(rng)
		enc, err := EncodeVC(vc)
		if err != nil {
			var oversize *OversizeError
			if asOversize(err, &oversize) {
				continue // legitimately oversize inputs are the cap working
			}
			t.Fatalf("iter %d: encode %+v: %v", i, vc, err)
		}
		if len(enc) > MaxEncodedBytes {
			t.Fatalf("iter %d: encoded %d bytes exceeds cap", i, len(enc))
		}
		got, err := DecodeVC(enc)
		if err != nil {
			t.Fatalf("iter %d: decode: %v", i, err)
		}
		if !got.Deadline.Equal(vc.Deadline) {
			t.Fatalf("iter %d: deadline drifted", i)
		}
		got.Deadline, vc.Deadline = time.Time{}, time.Time{}
		if got != vc {
			t.Fatalf("iter %d: drift\n got %+v\nwant %+v", i, got, vc)
		}
	}
}

// FuzzDecodeVC: arbitrary bytes must never panic the decoder, and
// anything that decodes must re-encode to an equivalent context.
func FuzzDecodeVC(f *testing.F) {
	seed, _ := EncodeVC(codecVC())
	f.Add(seed)
	f.Add("1|f|e|c|s|1|USD|2|fee|1|0")
	f.Add("")
	f.Add("1|")
	f.Fuzz(func(t *testing.T, s string) {
		vc, err := DecodeVC(s)
		if err != nil {
			return
		}
		enc, err := EncodeVC(vc)
		if err != nil {
			t.Fatalf("decoded ok but re-encode failed: %v (from %q)", err, s)
		}
		vc2, err := DecodeVC(enc)
		if err != nil {
			t.Fatalf("re-decode failed: %v", err)
		}
		if !vc2.Deadline.Equal(vc.Deadline) {
			t.Fatal("deadline drift through re-encode")
		}
		vc2.Deadline, vc.Deadline = time.Time{}, time.Time{}
		if vc2 != vc {
			t.Fatalf("re-encode drift: %+v vs %+v", vc2, vc)
		}
	})
}

func BenchmarkEncodeVC(b *testing.B) {
	vc := codecVC()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeVC(vc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeVC(b *testing.B) {
	enc, _ := EncodeVC(codecVC())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeVC(enc); err != nil {
			b.Fatal(err)
		}
	}
}
