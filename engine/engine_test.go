package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
)

type nullQuerier struct{}

func (nullQuerier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, query.ErrUnsupported
}
func (nullQuerier) QueryEvents(context.Context, query.EventQuery) (query.EventGroups, error) {
	return nil, query.ErrUnsupported
}
func (nullQuerier) Capabilities() query.Caps { return query.Caps{} }

func TestComputeAssemblesDeterministicLegs(t *testing.T) {
	events := []biz.Outcome{
		{At: win.From.Add(time.Minute), Stage: "capture", Result: biz.ResultFailed,
			VC: biz.ValueContext{Flow: "invoice.pay", EntityID: "inv_1", CustomerID: "h:c1", Segment: "smb",
				Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee}},
	}
	q := memq.New(memq.WithEvents(events), memq.WithCaps(query.Caps{Events: true})) // events-only
	req := Request{Window: win, Scope: Scope{}, Flows: []string{"invoice.pay"}}
	report, err := Compute(context.Background(), nil, q, req)
	if err != nil {
		t.Fatal(err)
	}
	// Realized computed from events.
	if report.Realized.ByCurrency["USD"] != 14900 {
		t.Fatalf("realized = %v, want USD 14900", report.Realized.ByCurrency)
	}
	// Customers computed from events.
	if report.Customers.Distinct != 1 {
		t.Fatalf("customers distinct = %d, want 1", report.Customers.Distinct)
	}
	// Deferred needs metrics; this events-only backend marks it unavailable.
	if len(report.Deferred.Caveats) == 0 {
		t.Fatal("deferred must be marked unavailable on an events-only backend")
	}
	// M7/M8 legs are honestly pending.
	if len(report.Unrealized.Notes) == 0 || report.Coverage.Unavailable == "" {
		t.Fatal("unrealized and coverage must state they are not yet computed")
	}
	if report.LibraryVersion == "" || report.GeneratedAt.IsZero() {
		t.Fatal("report must carry provenance")
	}
}

func TestComputeNoBackendMarksLegsUnavailable(t *testing.T) {
	// A backend serving neither signal: no report failure, every leg honestly
	// unavailable rather than a fabricated zero.
	report, err := Compute(context.Background(), nil, nullQuerier{}, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Realized.Caveats) == 0 || len(report.Deferred.Caveats) == 0 {
		t.Fatal("money legs must be marked unavailable")
	}
	if report.Customers.NotAvailableReason == "" {
		t.Fatal("customers must report NotAvailable")
	}
}

func TestEvidenceLabels(t *testing.T) {
	cases := []struct {
		name string
		e    Evidence
		want string
	}{
		{"deterministic", EvidenceDeterministic, "deterministic"},
		{"estimate", EvidenceEstimate, "estimate"},
		{"trust", EvidenceTrust, "trust"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if string(c.e) != c.want {
				t.Fatalf("Evidence %q, want %q", c.e, c.want)
			}
		})
	}
}
