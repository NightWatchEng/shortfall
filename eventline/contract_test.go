package eventline

import (
	"reflect"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
)

// TestLineTagsMatchTheWireContract pins the one respelling of the outcome
// event's field names that the compiler cannot check for us.
//
// Every other copy in the tree was replaced by the biz.Attr* constants, so a
// rename there is a compile error. A struct tag is a literal by language
// rule, so this decoder could silently stop matching what the exporters
// write — which is the failure mode that produced three different spellings
// of the same five facts in the first place.
func TestLineTagsMatchTheWireContract(t *testing.T) {
	want := map[string]string{
		"Flow":        biz.AttrFlow,
		"Stage":       biz.AttrStage,
		"Outcome":     biz.AttrOutcome,
		"EntityID":    biz.AttrEntityID,
		"CustomerID":  biz.AttrCustomerID,
		"AmountMinor": biz.AttrAmountMinor,
		"Currency":    biz.AttrCurrency,
		"Exponent":    biz.AttrExponent,
		"Kind":        biz.AttrValueKind,
		"Estimated":   biz.AttrAmountEst,
		"Segment":     biz.AttrSegment,
		"Source":      biz.AttrSource,
		"Err":         biz.AttrError,
		"TraceID":     biz.AttrTraceID,
	}
	rt := reflect.TypeOf(line{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		w, checked := want[f.Name]
		if !checked {
			// A field this test does not know about is either a new wire
			// field nobody added to the contract, or a deliberate alias.
			// Either way it must be decided, not defaulted.
			if f.Name != "SourceSys" {
				t.Errorf("field %s carries tag %q but is not in the contract map — add it to biz or to the exemption", f.Name, f.Tag.Get("json"))
			}
			continue
		}
		if got := f.Tag.Get("json"); got != w {
			t.Errorf("field %s decodes %q, the exporters write %q — the decoder has drifted from the wire contract", f.Name, got, w)
		}
	}
	for name := range want {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("contract names %s but the decoder has no such field", name)
		}
	}
}
