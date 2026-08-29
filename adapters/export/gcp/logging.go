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

const eventMarker = "biz.outcome"

// buildEventRecord renders one Outcome as a Cloud Logging structured entry.
// It carries the exact amount and the entity/customer ids, which ADR-0004
// keeps off metrics and on events.
func buildEventRecord(projectID string, o biz.Outcome) ([]byte, error) {
	rec := map[string]any{
		"time":             o.At.UTC().Format(time.RFC3339Nano),
		"severity":         "INFO",
		"event":            eventMarker,
		"biz.flow":         o.VC.Flow,
		"biz.stage":        o.Stage,
		"biz.outcome":      string(o.Result),
		"biz.entity.id":    o.VC.EntityID,
		"biz.customer.id":  o.VC.CustomerID,
		"biz.amount_minor": o.VC.Money.Amount,
		"biz.currency":     o.VC.Money.Currency,
		"biz.exponent":     o.VC.Money.Exponent,
		"biz.value.kind":   string(o.VC.Kind),
		"biz.amount.est":   o.VC.Estimated,
	}
	if o.VC.Segment != "" {
		rec["biz.segment"] = o.VC.Segment
	}
	if o.Source != "" {
		rec["source"] = o.Source
	}
	if o.Err != "" {
		rec["error"] = o.Err
	}
	if o.TraceID != "" {
		rec["trace.id"] = o.TraceID
		if projectID != "" {
			// The reserved key Cloud Logging correlates with Cloud Trace.
			rec["logging.googleapis.com/trace"] = fmt.Sprintf("projects/%s/traces/%s", projectID, o.TraceID)
		}
	}
	return json.Marshal(rec)
}
