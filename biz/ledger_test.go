// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package biz

import "testing"

func TestLedgerRowValidate(t *testing.T) {
	usd := func(amt int64) Money { return Money{Amount: amt, Currency: "USD", Exponent: 2} }
	cases := []struct {
		name string
		r    LedgerRow
		ok   bool
	}{
		{"valid success", LedgerRow{Flow: "checkout.pay", Outcome: ResultSuccess, Money: usd(14000), Count: 2}, true},
		{"valid failed", LedgerRow{Flow: "invoice.pay", Outcome: ResultFailed, Money: usd(9000), Count: 1}, true},
		{"valid deferred", LedgerRow{Outcome: ResultDeferred, Money: usd(3000), Count: 1}, true},
		{"zero-amount record is legitimate", LedgerRow{Outcome: ResultSuccess, Money: usd(0), Count: 1}, true},
		{"empty flow (unattributed) is allowed", LedgerRow{Flow: "", Outcome: ResultSuccess, Money: usd(100), Count: 1}, true},
		{"abandoned is not a ledger outcome", LedgerRow{Outcome: ResultAbandoned, Money: usd(100), Count: 1}, false},
		{"unknown is not a ledger outcome", LedgerRow{Outcome: ResultUnknown, Money: usd(100), Count: 1}, false},
		{"empty outcome rejected", LedgerRow{Outcome: "", Money: usd(100), Count: 1}, false},
		{"invalid money rejected", LedgerRow{Outcome: ResultSuccess, Money: Money{Amount: 100, Currency: "usd", Exponent: 2}, Count: 1}, false},
		{"negative amount rejected via money", LedgerRow{Outcome: ResultSuccess, Money: usd(-1), Count: 1}, false},
		{"negative count rejected", LedgerRow{Outcome: ResultSuccess, Money: usd(100), Count: -1}, false},
		{"money over zero records rejected", LedgerRow{Outcome: ResultSuccess, Money: usd(100), Count: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.r.Validate()
			if c.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}

			if !c.ok && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}
