// Package eventline decodes the exporters' shared biz.* outcome line — the
// JSON shape the Loki, CloudWatch EMF, and Splunk HEC exporters all write
// (docs/semconv.md) — back into a biz.Outcome. It is the read half of that
// convention: the log-store queriers (logql, cwinsights, spl) fetch raw
// lines, parse them here, and delegate aggregation to the memq reference.
package eventline

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// line mirrors the exporters' field set. Source has two spellings on the
// wire: Loki/EMF write "source", Splunk HEC writes "source_system" (HEC
// reserves "source" for its own envelope).
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
