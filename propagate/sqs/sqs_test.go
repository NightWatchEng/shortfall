package sqs

import (
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/propagate"
)

func vc() biz.ValueContext {
	return biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_2", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
}

func TestSQSCarrierRoundTrip(t *testing.T) {
	attrs := map[string]Attribute{}
	if err := propagate.Inject(NewCarrier(attrs), vc()); err != nil {
		t.Fatal(err)
	}
	if attrs[biz.MemberKey].DataType != "String" {
		t.Fatalf("injected attr must be a String type: %+v", attrs[biz.MemberKey])
	}
	got, ok, err := propagate.Extract(NewCarrier(attrs))
	if err != nil || !ok || got.EntityID != "inv_2" {
		t.Fatalf("round trip: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestSQSCases(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]Attribute
		key   string
		want  string
	}{
		{"present", map[string]Attribute{"x": {StringValue: "v"}}, "x", "v"},
		{"absent", map[string]Attribute{}, "x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NewCarrier(c.attrs).Get(c.key); got != c.want {
				t.Fatalf("get = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSQSNilSafe(t *testing.T) {
	c := NewCarrier(nil)
	c.Set("k", "v")
	if c.Get("k") != "" {
		t.Fatal("nil map carrier should be inert on set")
	}
}
