// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package propagate

import (
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
)

func carrierVC() biz.ValueContext {
	return biz.ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_42",
		CustomerID: "h:cc",
		Segment:    "smb",
		Money:      biz.Money{Amount: 500, Currency: "USD", Exponent: 2},
		Kind:       biz.KindFee,
	}
}

// mapCarrier is a trivial in-test Carrier for exercising Inject/Extract.
type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string  { return m[k] }
func (m mapCarrier) Set(k, v string) bool { m[k] = v; return true }
func (m mapCarrier) Keys() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func TestInjectExtractRoundTrip(t *testing.T) {
	c := mapCarrier{}
	if err := Inject(c, carrierVC()); err != nil {
		t.Fatalf("inject: %v", err)
	}

	if c[biz.MemberKey] == "" {
		t.Fatal("Inject wrote nothing under the biz.vc key")
	}

	got, ok, err := Extract(c)
	if err != nil || !ok {
		t.Fatalf("extract: ok=%v err=%v", ok, err)
	}

	if got.EntityID != "inv_42" || got.Money != carrierVC().Money {
		t.Fatalf("round trip drifted: %+v", got)
	}
}

func TestExtractOutcomes(t *testing.T) {
	valid := mapCarrier{}
	_ = Inject(valid, carrierVC())
	cases := []struct {
		name    string
		carrier mapCarrier
		wantOK  bool
		wantErr bool
	}{
		{"present and valid", valid, true, false},
		{"absent", mapCarrier{}, false, false},
		{"corrupt", mapCarrier{biz.MemberKey: "1|garbage"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok, err := Extract(c.carrier)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}

			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestInjectRejectsOversize(t *testing.T) {
	c := mapCarrier{}
	vc := carrierVC()
	vc.EntityID = ""
	for i := 0; i < 200; i++ {
		vc.EntityID += "xyz"
	}

	if err := Inject(c, vc); err == nil {
		t.Fatal("oversize ValueContext should fail to inject (cap is the codec's)")
	}
}

// nilCarrier models a carrier whose backing store cannot hold a write.
type nilCarrier struct{}

func (nilCarrier) Get(string) string       { return "" }
func (nilCarrier) Set(string, string) bool { return false }
func (nilCarrier) Keys() []string          { return nil }

func TestInjectFailsLoudlyWhenSetCannotWrite(t *testing.T) {
	if err := Inject(nilCarrier{}, carrierVC()); err == nil {
		t.Fatal("Inject over an unwritable carrier must return an error — silent propagation loss is forbidden")
	}
}
