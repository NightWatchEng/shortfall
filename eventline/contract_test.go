package eventline

import (
	"reflect"
	"testing"
	"time"

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
		"SLADeadline": biz.AttrSLADeadline,
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

// TestDeadlineRoundTrips pins the field this decoder was missing: the
// CloudWatch and GCP exporters write biz.sla.deadline, and a decoder that
// dropped it would lose the deadline on every round trip through a log
// store — silently, because the value simply arrives zero.
func TestDeadlineRoundTrips(t *testing.T) {
	const line = `{"event":"biz.outcome","biz.flow":"invoice.pay","biz.stage":"capture",` +
		`"biz.outcome":"failed","biz.entity.id":"inv_1","biz.customer.id":"h:c",` +
		`"biz.amount.minor":100,"biz.amount.currency":"USD","biz.amount.exponent":2,` +
		`"biz.value.kind":"fee","biz.amount.estimated":false,` +
		`"biz.sla.deadline":"2026-01-02T03:34:05Z"}`
	o, err := Parse([]byte(line), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := time.Date(2026, 1, 2, 3, 34, 5, 0, time.UTC)
	if !o.VC.Deadline.Equal(want) {
		t.Errorf("Deadline = %v, want %v — a deadline on the wire must survive the round trip", o.VC.Deadline, want)
	}
	// A line without one leaves it zero rather than erroring.
	noDeadline := `{"event":"biz.outcome","biz.flow":"f","biz.stage":"s","biz.outcome":"success",` +
		`"biz.entity.id":"e","biz.customer.id":"h:c","biz.amount.minor":1,"biz.amount.currency":"USD",` +
		`"biz.amount.exponent":2,"biz.value.kind":"fee","biz.amount.estimated":false}`
	o2, err := Parse([]byte(noDeadline), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("parse without deadline: %v", err)
	}
	if !o2.VC.Deadline.IsZero() {
		t.Errorf("Deadline = %v for a line that carries none, want zero", o2.VC.Deadline)
	}
}
