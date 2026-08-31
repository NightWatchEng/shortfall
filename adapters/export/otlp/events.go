// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package otlp

import (
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/NightWatchEng/shortfall/biz"
)

// eventName is the OTel semantic-convention event name for a shortfall
// outcome.
// The event MARKER, not the result attribute. The two share a value today,
// so binding this to biz.AttrOutcome was invisible — and renaming either one
// would have split the transports: the OTLP event name would move with the
// result attribute, or CloudWatch and GCP's "event" key would move without
// it. That is the drift this contract exists to prevent.
const eventName = biz.EventOutcome

// buildRecord maps one outcome to an OpenTelemetry log API record (the
// builder form — attribute limits are applied SDK-side, so all fields
// survive). Amounts and ids ride here (events), never on metrics. The
// record's timestamp is the outcome's own At; trace linking is applied by
// the emitter through the emit context (the log SDK links traces from
// ctx, not the record).
func buildRecord(o biz.Outcome) otellog.Record {
	var r otellog.Record
	r.SetEventName(eventName)
	r.SetTimestamp(o.At)
	r.SetObservedTimestamp(o.At)
	r.SetBody(attribute.StringValue(eventName))
	r.AddAttributes(outcomeAttrs(o)...)
	return r
}

func outcomeAttrs(o biz.Outcome) []attribute.KeyValue {
	kv := []attribute.KeyValue{
		attribute.String(biz.AttrFlow, o.VC.Flow),
		attribute.String(biz.AttrStage, o.Stage),
		attribute.String(biz.AttrOutcome, string(o.Result)),
		attribute.String(biz.AttrEntityID, o.VC.EntityID),
		attribute.String(biz.AttrCustomerID, o.VC.CustomerID),
		attribute.Int64(biz.AttrAmountMinor, o.VC.Money.Amount),
		attribute.String(biz.AttrCurrency, o.VC.Money.Currency),
		attribute.Int(biz.AttrExponent, int(o.VC.Money.Exponent)),
		attribute.String(biz.AttrValueKind, string(o.VC.Kind)),
		attribute.Bool(biz.AttrAmountEst, o.VC.Estimated),
	}
	if o.VC.Segment != "" {
		kv = append(kv, attribute.String(biz.AttrSegment, o.VC.Segment))
	}

	if !o.VC.Deadline.IsZero() {
		kv = append(kv, attribute.String(biz.AttrSLADeadline, o.VC.Deadline.UTC().Format("2006-01-02T15:04:05Z")))
	}

	if o.Source != "" {
		kv = append(kv, attribute.String(biz.AttrSource, o.Source))
	}

	if o.Err != "" {
		kv = append(kv, attribute.String(biz.AttrError, o.Err))
	}

	return kv
}
