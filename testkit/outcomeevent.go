package testkit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/NightWatchEng/shortfall/biz"
)

// OutcomeEventExtractor serializes one Outcome the way a transport does and
// returns the biz.* fields it produced, flattened to name -> value. An
// adapter supplies its own: JSON decode for the line-oriented exporters, an
// attribute walk for OTLP.
//
// Returning a map rather than raw bytes is what lets one vector hold every
// transport to the same field set while each keeps its own envelope — EMF's
// _aws block and Cloud Logging's severity are not part of this contract.
type OutcomeEventExtractor func(biz.Outcome) (map[string]any, error)

// CheckOutcomeEvent drives every vector case through an exporter and reports
// each disagreement with the wire contract. Empty means conformant.
//
// This is the test ADR-0002 has always claimed exists. It did not, and in
// its absence biz.sla.deadline shipped on OTLP alone — the same Outcome
// producing different fields depending on which exporter was wired.
func CheckOutcomeEvent(v OutcomeEventVectors, extract OutcomeEventExtractor) []string {
	var problems []string
	for _, c := range v.Cases {
		o, err := c.Input.Outcome()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: building the Outcome: %v", c.Name, err))
			continue
		}
		got, err := extract(o)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: exporting: %v", c.Name, err))
			continue
		}
		for _, name := range sortedKeys(c.Attrs) {
			want := c.Attrs[name]
			g, ok := got[name]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: %s missing — the contract requires it", c.Name, name))
				continue
			}
			if !sameWireValue(g, want) {
				problems = append(problems, fmt.Sprintf("%s: %s = %v, contract says %v", c.Name, name, g, want))
			}
		}
		for _, name := range c.Absent {
			if g, ok := got[name]; ok {
				problems = append(problems, fmt.Sprintf("%s: %s present as %v — the contract requires it absent, not empty", c.Name, name, g))
			}
		}
		// Anything in the biz.* namespace the contract does not name is an
		// addition nobody agreed to, and the next spelling to drift.
		for _, name := range sortedKeys(got) {
			if !strings.HasPrefix(name, "biz.") {
				continue
			}
			if _, named := c.Attrs[name]; named {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: %s is not in the contract — add it to biz and regenerate the vector, or stop emitting it", c.Name, name))
		}
	}
	return problems
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameWireValue compares across the type erosion a transport applies. A
// JSON round trip makes every number a float64 and OTLP's attribute API
// widens int8 to int64, so comparing Go types would fail on transports that
// are behaving correctly. Numbers are compared as numbers and everything
// else by its rendered form.
func sameWireValue(got, want any) bool {
	gn, gok := asNumber(got)
	wn, wok := asNumber(want)
	if gok && wok {
		return gn == wn
	}
	if gok != wok {
		return false
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
