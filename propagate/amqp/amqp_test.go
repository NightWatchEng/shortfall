package amqp

import (
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/propagate"
)

func vc() biz.ValueContext {
	return biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_3", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
}

func TestAMQPCarrierRoundTrip(t *testing.T) {
	table := map[string]interface{}{}
	if err := propagate.Inject(NewCarrier(table), vc()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := propagate.Extract(NewCarrier(table))
	if err != nil || !ok || got.EntityID != "inv_3" {
		t.Fatalf("round trip: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestAMQPGetAcceptsStringAndBytes(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  string
	}{
		{"string", "v", "v"},
		{"bytes", []byte("v"), "v"},
		{"other type ignored", 42, ""},
		{"absent", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			table := map[string]interface{}{}
			if c.value != nil {
				table["k"] = c.value
			}
			if got := NewCarrier(table).Get("k"); got != c.want {
				t.Fatalf("get = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAMQPNilSafe(t *testing.T) {
	c := NewCarrier(nil)
	if c.Set("k", "v") {
		t.Fatal("Set on a nil carrier must report false")
	}
	if c.Get("k") != "" {
		t.Fatal("nil table carrier should be inert on set")
	}
}
