package biz_test

import (
	"context"
	"fmt"

	"github.com/NightWatchEng/shortfall/biz"
)

// Attach business context to a request at the edge; anything downstream —
// middleware, queue consumers, the emitter — reads it back from the context.
func ExampleWithValueContext() {
	vc := biz.ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_00000042",
		CustomerID: "h:c000007", // pre-hashed — raw ids never enter biz.*
		Segment:    "smb",
		Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
		Kind:       biz.KindFee,
	}
	ctx, err := biz.WithValueContext(context.Background(), vc)
	if err != nil {
		fmt.Println("invalid context:", err)
		return
	}

	got, ok, _ := biz.FromContext(ctx)
	fmt.Println(ok, got.Flow, got.Money)
	// Output: true invoice.pay USD 49.99
}

// Money is int64 minor units plus currency and exponent — never a float.
func ExampleMoney_String() {
	fmt.Println(biz.Money{Amount: 1250000, Currency: "USD", Exponent: 2})
	fmt.Println(biz.Money{Amount: 5000, Currency: "JPY", Exponent: 0})
	// Output:
	// USD 12500.00
	// JPY 5000
}
