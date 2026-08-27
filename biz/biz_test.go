package biz

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMoneyValidate(t *testing.T) {
	cases := []struct {
		name string
		m    Money
		ok   bool
	}{
		{"usd cents", Money{Amount: 14900, Currency: "USD", Exponent: 2}, true},
		{"jpy zero exponent", Money{Amount: 14900, Currency: "JPY", Exponent: 0}, true},
		{"bhd three places", Money{Amount: 1000, Currency: "BHD", Exponent: 3}, true},
		{"zero amount ok", Money{Amount: 0, Currency: "USD", Exponent: 2}, true},
		{"negative amount", Money{Amount: -1, Currency: "USD", Exponent: 2}, false},
		{"lowercase currency", Money{Amount: 1, Currency: "usd", Exponent: 2}, false},
		{"two letter currency", Money{Amount: 1, Currency: "US", Exponent: 2}, false},
		{"empty currency", Money{Amount: 1, Currency: "", Exponent: 2}, false},
		{"negative exponent", Money{Amount: 1, Currency: "USD", Exponent: -1}, false},
		{"absurd exponent", Money{Amount: 1, Currency: "USD", Exponent: 5}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.Validate()
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("want error, got none: %+v", c.m)
			}
		})
	}
}

func TestMoneyString(t *testing.T) {
	cases := []struct {
		m    Money
		want string
	}{
		{Money{Amount: 14900, Currency: "USD", Exponent: 2}, "USD 149.00"},
		{Money{Amount: 14900, Currency: "JPY", Exponent: 0}, "JPY 14900"},
		{Money{Amount: 1001, Currency: "BHD", Exponent: 3}, "BHD 1.001"},
		{Money{Amount: 5, Currency: "USD", Exponent: 2}, "USD 0.05"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.m.String(); got != c.want {
				t.Errorf("%+v.String() = %q, want %q", c.m, got, c.want)
			}
		})
	}
}

func TestParseMinor(t *testing.T) {
	cases := []struct {
		s    string
		exp  int8
		want int64
		ok   bool
	}{
		{"149.00", 2, 14900, true},
		{"149", 2, 14900, true},
		{"149.5", 2, 14950, true},
		{"0.05", 2, 5, true},
		{"14900", 0, 14900, true},
		{"1.001", 3, 1001, true},
		{"149.005", 2, 0, false}, // excess precision must never round silently
		{"149.", 2, 0, false},
		{".5", 2, 0, false},
		{"-1.00", 2, 0, false},
		{"1,000.00", 2, 0, false},
		{"", 2, 0, false},
		{"abc", 2, 0, false},
		{"14900.0", 0, 0, false}, // JPY has no decimals
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q@%d", c.s, c.exp), func(t *testing.T) {
			got, err := ParseMinor(c.s, c.exp)
			if c.ok && (err != nil || got != c.want) {
				t.Errorf("ParseMinor(%q, %d) = %d, %v; want %d", c.s, c.exp, got, err, c.want)
			}
			if !c.ok && err == nil {
				t.Errorf("ParseMinor(%q, %d) = %d, want error", c.s, c.exp, got)
			}
		})
	}
}

func TestEnums(t *testing.T) {
	kinds := []struct {
		k    Kind
		want bool
	}{
		{KindGMV, true}, {KindNetRevenue, true}, {KindFee, true}, {KindTakeRate, true},
		{Kind("revenue"), false}, {Kind(""), false},
	}
	for _, c := range kinds {
		t.Run("kind "+string(c.k), func(t *testing.T) {
			if c.k.Valid() != c.want {
				t.Errorf("Kind(%q).Valid() = %v, want %v", c.k, !c.want, c.want)
			}
		})
	}
	results := []struct {
		r    Result
		want bool
	}{
		{ResultSuccess, true}, {ResultFailed, true}, {ResultDeferred, true},
		{ResultAbandoned, true}, {ResultUnknown, true},
		{Result("maybe"), false}, {Result(""), false},
	}
	for _, c := range results {
		t.Run("result "+string(c.r), func(t *testing.T) {
			if c.r.Valid() != c.want {
				t.Errorf("Result(%q).Valid() = %v, want %v", c.r, !c.want, c.want)
			}
		})
	}
}

func validVC() ValueContext {
	return ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_8Ka92j",
		CustomerID: "h:3f9ac2",
		Segment:    "smb",
		Money:      Money{Amount: 14900, Currency: "USD", Exponent: 2},
		Kind:       KindFee,
	}
}

func TestValueContextValidate(t *testing.T) {
	if err := validVC().Validate(); err != nil {
		t.Fatalf("valid VC rejected: %v", err)
	}

	mutate := func(f func(*ValueContext)) ValueContext {
		vc := validVC()
		f(&vc)
		return vc
	}
	cases := []struct {
		name string
		vc   ValueContext
	}{
		{"empty flow", mutate(func(v *ValueContext) { v.Flow = "" })},
		{"flow bad charset", mutate(func(v *ValueContext) { v.Flow = "Invoice Pay!" })},
		{"flow too long", mutate(func(v *ValueContext) { v.Flow = strings.Repeat("a", 65) })},
		{"empty entity", mutate(func(v *ValueContext) { v.EntityID = "" })},
		{"entity too long", mutate(func(v *ValueContext) { v.EntityID = strings.Repeat("x", 129) })},
		{"customer too long", mutate(func(v *ValueContext) { v.CustomerID = strings.Repeat("x", 129) })},
		{"segment too long", mutate(func(v *ValueContext) { v.Segment = strings.Repeat("s", 33) })},
		{"bad kind", mutate(func(v *ValueContext) { v.Kind = "profit" })},
		{"bad money", mutate(func(v *ValueContext) { v.Money.Currency = "dollars" })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.vc.Validate(); err == nil {
				t.Fatalf("want error, got none: %+v", c.vc)
			}
		})
	}

	// Deadline is optional; a set one is fine.
	vc := validVC()
	vc.Deadline = time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	if err := vc.Validate(); err != nil {
		t.Fatalf("VC with deadline rejected: %v", err)
	}
}

func TestPIIGuard(t *testing.T) {
	reject := []struct {
		name, value string
	}{
		// The classic acceptance case: a Luhn-valid 16-digit PAN.
		{"visa test pan", "4111111111111111"},
		{"pan with dashes", "4111-1111-1111-1111"},
		{"pan with spaces", "4111 1111 1111 1111"},
		{"amex 15 digit", "378282246310005"},
		{"pan embedded", "cust-4111111111111111-x"},
		{"email", "jane.doe@example.com"},
		{"email embedded", "id+jane.doe@example.com+42"},
		{"iban", "DE89370400440532013000"},
		{"iban gb", "GB82WEST12345698765432"},
	}
	for _, c := range reject {
		t.Run("customer "+c.name, func(t *testing.T) {
			vc := validVC()
			vc.CustomerID = c.value
			if err := vc.Validate(); err == nil {
				t.Fatalf("PII in CustomerID accepted: %q", c.value)
			}
		})
		t.Run("entity "+c.name, func(t *testing.T) {
			vc := validVC()
			vc.EntityID = c.value
			if err := vc.Validate(); err == nil {
				t.Fatalf("PII in EntityID accepted: %q", c.value)
			}
		})
	}

	accept := []struct {
		name, value string
	}{
		// Luhn-INVALID 16 digits: not a PAN, must pass (order ids exist).
		{"luhn-invalid digits", "4111111111111112"},
		{"short digits", "123456789012"},        // 12 digits: below PAN range
		{"long digits", "12345678901234567890"}, // 20 digits: above PAN range
		{"hashed id", "h:3f9ac2b871"},
		{"uuid-ish", "550e8400-e29b-41d4-a716-446655440000"},
		{"invoice id", "inv_8Ka92j"},
	}
	for _, c := range accept {
		t.Run("accepts "+c.name, func(t *testing.T) {
			vc := validVC()
			vc.CustomerID = c.value
			if err := vc.Validate(); err != nil {
				t.Fatalf("non-PII rejected: %q: %v", c.value, err)
			}
		})
	}
}

func TestOutcomeValidate(t *testing.T) {
	ok := Outcome{
		At:     time.Date(2026, 8, 27, 14, 32, 0, 0, time.UTC),
		VC:     validVC(),
		Stage:  "capture",
		Result: ResultFailed,
		Source: "stripe:webhook",
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid outcome rejected: %v", err)
	}
	bad := []struct {
		name string
		f    func(*Outcome)
	}{
		{"zero time", func(o *Outcome) { o.At = time.Time{} }},
		{"empty stage", func(o *Outcome) { o.Stage = "" }},
		{"stage charset", func(o *Outcome) { o.Stage = "Cap ture!" }},
		{"bad result", func(o *Outcome) { o.Result = "maybe" }},
		{"bad vc", func(o *Outcome) { o.VC.Flow = "" }},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			o := ok
			c.f(&o)
			if err := o.Validate(); err == nil {
				t.Fatal("want error, got none")
			}
		})
	}
}

func BenchmarkValueContextValidate(b *testing.B) {
	vc := validVC()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := vc.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestOutcomeFreeTextFieldsAreGuarded(t *testing.T) {
	base := Outcome{
		At:     time.Date(2026, 8, 27, 14, 32, 0, 0, time.UTC),
		VC:     validVC(),
		Stage:  "capture",
		Result: ResultFailed,
	}
	cases := []struct {
		name string
		f    func(*Outcome)
	}{
		{"pan in err", func(o *Outcome) { o.Err = "card 4111111111111111 declined" }},
		{"pan with spaces in err", func(o *Outcome) { o.Err = "card 4111 1111 1111 1111 declined" }},
		{"email in err", func(o *Outcome) { o.Err = "declined for jane.doe@example.com" }},
		{"email in source", func(o *Outcome) { o.Source = "jane.doe@example.com" }},
		{"oversize err", func(o *Outcome) { o.Err = strings.Repeat("x", 513) }},
		{"trace id wrong length", func(o *Outcome) { o.TraceID = "abc123" }},
		{"trace id uppercase", func(o *Outcome) { o.TraceID = strings.Repeat("A", 32) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := base
			c.f(&o)
			if err := o.Validate(); err == nil {
				t.Fatal("want error, got none")
			}
		})
	}
	ok := base
	ok.Err = "capture timeout after 30s"
	ok.Source = "stripe:webhook"
	ok.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	if err := ok.Validate(); err != nil {
		t.Fatalf("benign free-text fields rejected: %v", err)
	}
}

func TestCheckPIIExportedSurface(t *testing.T) {
	// The exported guard is the reuse point for emit/registry free text:
	// space-separated PANs are legal characters there, so the scanner's
	// space branch must be live through this path.
	if err := CheckPII("probe", "card 4111 1111 1111 1111 declined"); err == nil {
		t.Fatal("space-separated PAN passed CheckPII")
	} else if !strings.Contains(err.Error(), "card number") {
		t.Fatalf("wrong rejection reason: %v", err)
	}
	if err := CheckPII("probe", "all clear here"); err != nil {
		t.Fatalf("benign text rejected: %v", err)
	}
}

func TestPANBoundariesAndSubSegments(t *testing.T) {
	reject := []string{
		"4000000000006",        // 13-digit boundary, Luhn-valid
		"4000000000000000006",  // 19-digit boundary, Luhn-valid
		"x-9-4111111111111111", // PAN hidden behind one dash-joined stray digit
	}
	for _, v := range reject {
		t.Run(v, func(t *testing.T) {
			vc := validVC()
			vc.CustomerID = v
			if err := vc.Validate(); err == nil {
				t.Errorf("PAN shape accepted: %q", v)
			}
		})
	}
	// One unbroken over-length run must NOT fire on a Luhn-valid substring.
	t.Run("unbroken 20-digit id passes", func(t *testing.T) {
		vc := validVC()
		vc.CustomerID = "12345678901234567890"
		if err := vc.Validate(); err != nil {
			t.Errorf("20-digit unbroken id rejected: %v", err)
		}
	})
}

func TestIBANCaseAndBoundaryHardening(t *testing.T) {
	reject := []string{
		"DE89370400440532013000",
		"xDE89370400440532013000", // abutting letter must not hide it
		"de89370400440532013000",  // lowercase must not hide it
	}
	for _, v := range reject {
		t.Run(v, func(t *testing.T) {
			vc := validVC()
			vc.EntityID = v
			if err := vc.Validate(); err == nil {
				t.Errorf("IBAN shape accepted: %q", v)
			}
		})
	}
	// Shape match with a FAILING mod-97 checksum is an id, not an IBAN.
	t.Run("mod-97-invalid shape passes", func(t *testing.T) {
		vc := validVC()
		vc.EntityID = "DE00370400440532013001"
		if err := vc.Validate(); err != nil {
			t.Errorf("mod-97-invalid shape rejected: %v", err)
		}
	})
}

func TestParseMinorOverflowGuards(t *testing.T) {
	cases := []struct {
		s    string
		exp  int8
		want int64
		ok   bool
	}{
		{"9223372036854775807", 0, 9223372036854775807, true},
		{"9223372036854775808", 0, 0, false},
		{"92233720368547758.07", 2, 9223372036854775807, true},
		{"92233720368547758.08", 2, 0, false},
		{"99999999999999999999999", 2, 0, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q@%d", c.s, c.exp), func(t *testing.T) {
			got, err := ParseMinor(c.s, c.exp)
			if c.ok && (err != nil || got != c.want) {
				t.Errorf("ParseMinor(%q, %d) = %d, %v; want %d", c.s, c.exp, got, err, c.want)
			}
			if !c.ok && err == nil {
				t.Errorf("ParseMinor(%q, %d) = %d, want overflow error", c.s, c.exp, got)
			}
		})
	}
}

func TestMoneyStringIsTotal(t *testing.T) {
	if got := (Money{Amount: 9223372036854775807, Currency: "USD", Exponent: 2}).String(); got != "USD 92233720368547758.07" {
		t.Errorf("MaxInt64 render = %q", got)
	}
	// Invalid receivers render marked, never panic, never garbage signs.
	invalid := []struct {
		name string
		m    Money
	}{
		{"pow-wrapping exponent", Money{Amount: 1, Currency: "USD", Exponent: 64}},
		{"negative amount", Money{Amount: -105, Currency: "USD", Exponent: 2}},
		{"lowercase currency", Money{Amount: 1, Currency: "usd", Exponent: 2}},
	}
	for _, c := range invalid {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.String(); !strings.Contains(got, "INVALID") {
				t.Errorf("invalid money %+v rendered %q, want marked INVALID form", c.m, got)
			}
		})
	}
}
