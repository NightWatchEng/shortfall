// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package testkit

import (
	"path/filepath"
	"testing"

	"github.com/NightWatchEng/shortfall/biz"
)

func loadOutcomeVectors(t *testing.T) OutcomeEventVectors {
	t.Helper()
	v, err := LoadOutcomeEventVectors(filepath.Join(VectorsDir, OutcomeEventVectorsFile))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return v
}

// TestOutcomeEventVectorNamesAreUnique is the guard Names() exists for. Two
// cases sharing a name would make a failure ambiguous about which input
// produced it.
func TestOutcomeEventVectorNamesAreUnique(t *testing.T) {
	assertUniqueNames(t, loadOutcomeVectors(t).Names())
}

// TestCheckOutcomeEventDetects drives the checker with deliberately
// non-conformant extractors. Without this the checker's failure paths are
// asserted nowhere: the three adapter contract tests only ever feed it a
// conformant exporter, so a checker that returned nil unconditionally would
// leave every one of them green.
func TestCheckOutcomeEventDetects(t *testing.T) {
	v := loadOutcomeVectors(t)
	// conformant is what a correct exporter produces, derived from the
	// vector itself so this test cannot drift from the contract.
	conformant := func(c OutcomeEventVector) map[string]any {
		m := map[string]any{}
		for k, val := range c.Attrs {
			m[k] = val
		}
		for k, val := range c.AttrsIfCarried {
			m[k] = val
		}
		return m
	}
	byName := map[string]OutcomeEventVector{}
	for _, c := range v.Cases {
		byName[c.Name] = c
	}
	full, ok := byName["fully_populated"]
	if !ok {
		t.Fatal("the vector no longer has a fully_populated case")
	}

	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantSub string
	}{
		{"required field missing", func(m map[string]any) { delete(m, biz.AttrFlow) }, "biz.flow missing"},
		{"wrong value", func(m map[string]any) { m[biz.AttrCurrency] = "XXX" }, "biz.amount.currency"},
		{"extra biz field", func(m map[string]any) { m["biz.invented"] = 1 }, "not in the contract"},
		{"bool as string", func(m map[string]any) { m[biz.AttrAmountEst] = "false" }, "(string), contract says"},
		{"number as string", func(m map[string]any) { m[biz.AttrAmountMinor] = "14900" }, "biz.amount.minor"},
		{"natively-carried field dropped", func(m map[string]any) { delete(m, biz.AttrTraceID) }, "trace.id missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			one := OutcomeEventVectors{VectorsVersion: v.VectorsVersion, Cases: []OutcomeEventVector{full}}
			problems := CheckOutcomeEvent(one, func(biz.Outcome) (map[string]any, error) {
				m := conformant(full)
				tc.mutate(m)
				return m, nil
			})
			if len(problems) == 0 {
				t.Fatalf("checker reported conformant, want a complaint containing %q", tc.wantSub)
			}
			var joined string
			for _, p := range problems {
				joined += p + "\n"
			}
			if !contains(joined, tc.wantSub) {
				t.Errorf("complaints did not mention %q:\n%s", tc.wantSub, joined)
			}
		})
	}

	// And the control: an exporter that matches the vector is silent.
	for _, c := range v.Cases {
		one := OutcomeEventVectors{VectorsVersion: v.VectorsVersion, Cases: []OutcomeEventVector{c}}
		if p := CheckOutcomeEvent(one, func(biz.Outcome) (map[string]any, error) { return conformant(c), nil }); len(p) != 0 {
			t.Errorf("%s: a conformant extractor was reported non-conformant: %v", c.Name, p)
		}
	}
}

// TestCheckOutcomeEventSurfacesExtractorFailure: an exporter that errors must
// be reported, not treated as conformant.
func TestCheckOutcomeEventSurfacesExtractorFailure(t *testing.T) {
	v := loadOutcomeVectors(t)
	problems := CheckOutcomeEvent(v, func(biz.Outcome) (map[string]any, error) {
		return nil, errBoom
	})
	if len(problems) != len(v.Cases) {
		t.Errorf("got %d problems for %d failing cases: %v", len(problems), len(v.Cases), problems)
	}
}

var errBoom = errString("collector unreachable")

type errString string

func (e errString) Error() string { return string(e) }

func contains(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
