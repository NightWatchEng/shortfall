package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/NightWatchEng/shortfall/query"
)

type nullQuerier struct{}

func (nullQuerier) QueryMetric(context.Context, query.Query) (query.Series, error) {
	return nil, nil
}
func (nullQuerier) QueryEvents(context.Context, query.EventQuery) (query.EventGroups, error) {
	return nil, query.ErrUnsupported
}
func (nullQuerier) Capabilities() query.Caps { return query.Caps{} }

func TestComputeIsLoudlyUnimplemented(t *testing.T) {
	// The freeze declares the signature; the legs land in M6/M7. Until
	// then a zero-filled report during an incident would be a lie, so
	// Compute must refuse.
	req := Request{Scope: Scope{"stage": "capture"}, Flows: []string{"invoice.pay"}}
	report, err := Compute(context.Background(), nil, nullQuerier{}, req)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Compute err = %v, want ErrNotImplemented", err)
	}
	if report.Request.Flows[0] != "invoice.pay" {
		t.Fatal("report must echo the request even when refusing")
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
