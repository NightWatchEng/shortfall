package kafka

import (
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/propagate"
)

func vc() biz.ValueContext {
	return biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_1", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
}

func TestKafkaCarrierRoundTrip(t *testing.T) {
	var headers []Header
	c := NewCarrier(&headers)
	if err := propagate.Inject(c, vc()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := propagate.Extract(NewCarrier(&headers))
	if err != nil || !ok || got.EntityID != "inv_1" {
		t.Fatalf("round trip: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestKafkaCarrierOps(t *testing.T) {
	headers := []Header{{Key: "trace", Value: []byte("t1")}}
	c := NewCarrier(&headers)
	cases := []struct {
		name string
		do   func()
		want func(t *testing.T)
	}{
		{"get existing", func() {}, func(t *testing.T) {
			if c.Get("trace") != "t1" {
				t.Fatal("get")
			}
		}},
		{"get missing empty", func() {}, func(t *testing.T) {
			if c.Get("nope") != "" {
				t.Fatal("missing should be empty")
			}
		}},
		{"set appends", func() { c.Set("k", "v") }, func(t *testing.T) {
			if c.Get("k") != "v" || len(headers) != 2 {
				t.Fatalf("append: %v", headers)
			}
		}},
		{"set replaces not duplicates", func() { c.Set("k", "v2") }, func(t *testing.T) {
			if c.Get("k") != "v2" || len(headers) != 2 {
				t.Fatalf("replace: %v", headers)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.do()
			tc.want(t)
		})
	}
	keys := c.Keys()
	if len(keys) != 2 || keys[0] != "trace" {
		t.Fatalf("keys order: %v", keys)
	}
}

func TestKafkaNilSafe(t *testing.T) {
	c := NewCarrier(nil)
	c.Set("k", "v") // must not panic
	if c.Get("k") != "" || c.Keys() != nil {
		t.Fatal("nil carrier should be inert")
	}
}
