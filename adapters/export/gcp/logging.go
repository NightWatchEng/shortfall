// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// Cloud Logging turns a JSON line on stdout into a structured entry: `time`
// and `severity` are lifted into the entry's own fields and everything else
// becomes jsonPayload. The payload field names mirror the CloudWatch EMF
// record (adapters/export/cloudwatch) field for field, so a reader who knows
// one backend can read the other and one query shape serves both.
//
// Severity is always INFO. A failed payment is a business outcome, not a
// logging error, and encoding the result as a severity would let a routine
// log-level filter hide exactly the events the realized-loss leg is
// computed from — the outcome rides the payload, where queries read it.

const eventMarker = biz.EventOutcome

// buildEventRecord renders one Outcome as a Cloud Logging structured entry.
// It carries the exact amount and the entity/customer ids, which ADR-0004
// keeps off metrics and on events.
func buildEventRecord(projectID string, o biz.Outcome) ([]byte, error) {
	rec := map[string]any{
		"time":              o.At.UTC().Format(time.RFC3339Nano),
		"severity":          "INFO",
		biz.EventKey:        eventMarker,
		biz.AttrFlow:        o.VC.Flow,
		biz.AttrStage:       o.Stage,
		biz.AttrOutcome:     string(o.Result),
		biz.AttrEntityID:    o.VC.EntityID,
		biz.AttrCustomerID:  o.VC.CustomerID,
		biz.AttrAmountMinor: o.VC.Money.Amount,
		biz.AttrCurrency:    o.VC.Money.Currency,
		biz.AttrExponent:    o.VC.Money.Exponent,
		biz.AttrValueKind:   string(o.VC.Kind),
		biz.AttrAmountEst:   o.VC.Estimated,
	}
	if o.VC.Segment != "" {
		rec[biz.AttrSegment] = o.VC.Segment
	}

	// The deadline rides every transport that can express it. It was on the
	// OTLP event alone, which made the same Outcome produce different fields
	// depending on which exporter was wired — exactly what ADR-0002 says
	// must not happen.
	if !o.VC.Deadline.IsZero() {
		rec[biz.AttrSLADeadline] = o.VC.Deadline.UTC().Format("2006-01-02T15:04:05Z")
	}

	if o.Source != "" {
		rec[biz.AttrSource] = o.Source
	}

	if o.Err != "" {
		rec[biz.AttrError] = o.Err
	}

	if o.TraceID != "" {
		rec[biz.AttrTraceID] = o.TraceID
		if projectID != "" {
			// The reserved key Cloud Logging correlates with Cloud Trace.
			rec["logging.googleapis.com/trace"] = fmt.Sprintf("projects/%s/traces/%s", projectID, o.TraceID)
		}
	}

	return json.Marshal(rec)
}
