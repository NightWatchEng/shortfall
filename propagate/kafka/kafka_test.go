// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

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

func TestKafkaCarrierGetSet(t *testing.T) {
	cases := []struct {
		name    string
		initial []Header
		setKey  string
		setVal  string // "" means do not call Set
		getKey  string
		wantGet string
		wantLen int
	}{
		{"get existing", []Header{{Key: "trace", Value: []byte("t1")}}, "", "", "trace", "t1", 1},
		{"get missing is empty", []Header{{Key: "trace", Value: []byte("t1")}}, "", "", "nope", "", 1},
		{"set appends new key", []Header{{Key: "trace", Value: []byte("t1")}}, "k", "v", "k", "v", 2},
		{"set replaces existing without duplicating", []Header{{Key: "k", Value: []byte("old")}}, "k", "v2", "k", "v2", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			headers := append([]Header(nil), c.initial...)
			cr := NewCarrier(&headers)
			if c.setVal != "" {
				if !cr.Set(c.setKey, c.setVal) {
					t.Fatal("Set on a valid carrier must report true")
				}
			}
			if got := cr.Get(c.getKey); got != c.wantGet {
				t.Fatalf("Get(%q) = %q, want %q", c.getKey, got, c.wantGet)
			}
			if len(headers) != c.wantLen {
				t.Fatalf("len = %d, want %d: %v", len(headers), c.wantLen, headers)
			}
		})
	}
}

func TestKafkaSetCanonicalizesDuplicates(t *testing.T) {
	// Kafka permits duplicate keys; Set must leave exactly one, so a
	// consumer reads one unambiguous value regardless of its client's
	// first-wins/last-wins semantics.
	headers := []Header{
		{Key: "biz.vc", Value: []byte("STALE_1")},
		{Key: "trace", Value: []byte("t")},
		{Key: "biz.vc", Value: []byte("STALE_2")},
	}
	cr := NewCarrier(&headers)
	cr.Set("biz.vc", "FRESH")
	count, val := 0, ""
	for _, h := range headers {
		if h.Key == "biz.vc" {
			count++
			val = string(h.Value)
		}
	}
	if count != 1 || val != "FRESH" {
		t.Fatalf("expected one canonical biz.vc=FRESH, got count=%d val=%q headers=%v", count, val, headers)
	}
}

func TestKafkaNilSafe(t *testing.T) {
	c := NewCarrier(nil)
	if c.Set("k", "v") {
		t.Fatal("Set on a nil carrier must report false")
	}
	if c.Get("k") != "" || c.Keys() != nil {
		t.Fatal("nil carrier should be inert")
	}
}
