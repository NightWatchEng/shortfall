// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

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
// compiler is what keeps the copy honest .
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
	AmountMinor json.RawMessage `json:"biz.amount.minor"`
	Currency    string          `json:"biz.amount.currency"`
	Exponent    int8            `json:"biz.amount.exponent"`
	Kind        string          `json:"biz.value.kind"`
	Estimated   bool            `json:"biz.amount.estimated"`
	Segment     string          `json:"biz.segment"`
	SLADeadline string          `json:"biz.sla.deadline"`
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
		return biz.Outcome{}, fmt.Errorf("eventline: outcome line carries no biz.amount.minor")
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
	// The deadline is optional on the wire and only some flows carry one.
	// A line that has it must round-trip it: the CloudWatch and GCP exporters
	// both write it now, and a decoder that silently dropped it would put this
	// package back on the wrong side of the contract biz/semconv.go exists to
	// hold everything to.
	var deadline time.Time
	if l.SLADeadline != "" {
		d, err := time.Parse(time.RFC3339, l.SLADeadline)
		if err != nil {
			return biz.Outcome{}, fmt.Errorf("eventline: %s %q: %w", biz.AttrSLADeadline, l.SLADeadline, err)
		}
		deadline = d
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
			Deadline:   deadline,
			Kind:       biz.Kind(l.Kind),
			Estimated:  l.Estimated,
			Money:      biz.Money{Amount: amount, Currency: l.Currency, Exponent: l.Exponent},
		},
	}, nil
}
