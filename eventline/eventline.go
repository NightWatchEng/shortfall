// Package eventline decodes the exporters' shared biz.* outcome line — the
// JSON shape the CloudWatch EMF exporter writes (docs/semconv.md) — back
// into a biz.Outcome. It is the read half of that convention: the
// log-store querier (cwinsights) fetches raw lines, parses them here, and
// delegates aggregation to the memq reference.
package eventline

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// line mirrors the exporter's field set. Its json tags must equal the
// biz.Attr* constants, and TestLineTagsMatchTheWireContract asserts exactly
// that: struct tags cannot reference a constant, so this is the one place
// the wire names are unavoidably respelled, and a test rather than the
// compiler is what keeps the copy honest (workspace-cnz).
//
// Source has two accepted
// spellings on the wire: "source" (what the EMF exporter writes) and
// "source_system" (kept for records ingested by since-removed exporters
// whose envelope reserved "source").
type line struct {
	Flow        string          `json:"biz.flow"`
	Stage       string          `json:"biz.stage"`
	Outcome     string          `json:"biz.outcome"`
	EntityID    string          `json:"biz.entity.id"`
	CustomerID  string          `json:"biz.customer.id"`
	AmountMinor json.RawMessage `json:"biz.amount_minor"`
	Currency    string          `json:"biz.currency"`
	Exponent    int8            `json:"biz.exponent"`
	Kind        string          `json:"biz.value.kind"`
	Estimated   bool            `json:"biz.amount.est"`
	Segment     string          `json:"biz.segment"`
	Source      string          `json:"source"`
	SourceSys   string          `json:"source_system"`
	Err         string          `json:"error"`
	TraceID     string          `json:"trace.id"`
}

// Parse decodes one outcome line, stamping it with the store's timestamp
// for the entry (the line itself carries no time — the log store owns it).
// A line without the biz.flow and biz.outcome markers is not an outcome
// event and is rejected so callers can skip foreign log lines loudly.
func Parse(raw []byte, at time.Time) (biz.Outcome, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return biz.Outcome{}, fmt.Errorf("eventline: parse: %w", err)
	}
	if l.Flow == "" || l.Outcome == "" {
		return biz.Outcome{}, fmt.Errorf("eventline: not a biz outcome line")
	}
	if !biz.Result(l.Outcome).Valid() {
		return biz.Outcome{}, fmt.Errorf("eventline: outcome %q is not a valid result", l.Outcome)
	}
	// The exporters always write the amount; a marked line without one is a
	// truncated or foreign record, and counting it as $0 would skew sums.
	if len(l.AmountMinor) == 0 {
		return biz.Outcome{}, fmt.Errorf("eventline: outcome line carries no biz.amount_minor")
	}
	var amount int64
	if len(l.AmountMinor) > 0 {
		// Decode via json.Number so a fractional or overflowing amount fails
		// loudly instead of truncating money.
		var n json.Number
		if err := json.Unmarshal(l.AmountMinor, &n); err != nil {
			return biz.Outcome{}, fmt.Errorf("eventline: amount_minor: %w", err)
		}
		v, err := n.Int64()
		if err != nil {
			return biz.Outcome{}, fmt.Errorf("eventline: amount_minor %q is not an int64: %w", n, err)
		}
		amount = v
	}
	source := l.Source
	if source == "" {
		source = l.SourceSys
	}
	return biz.Outcome{
		At:      at,
		Stage:   l.Stage,
		Result:  biz.Result(l.Outcome),
		Source:  source,
		Err:     l.Err,
		TraceID: l.TraceID,
		VC: biz.ValueContext{
			Flow:       l.Flow,
			EntityID:   l.EntityID,
			CustomerID: l.CustomerID,
			Segment:    l.Segment,
			Kind:       biz.Kind(l.Kind),
			Estimated:  l.Estimated,
			Money:      biz.Money{Amount: amount, Currency: l.Currency, Exponent: l.Exponent},
		},
	}, nil
}
