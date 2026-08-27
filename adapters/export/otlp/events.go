package otlp

import (
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/NightWatchEng/shortfall/biz"
)

// eventName is the OTel semantic-convention event name for a shortfall
// outcome (proposal 4.4).
const eventName = "biz.outcome"

// buildRecord maps one outcome to an OpenTelemetry log API record (the
// builder form — attribute limits are applied SDK-side, so all fields
// survive). Amounts and ids ride HERE (events), never on metrics. The
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
		attribute.String("biz.flow", o.VC.Flow),
		attribute.String("biz.stage", o.Stage),
		attribute.String("biz.outcome", string(o.Result)),
		attribute.String("biz.entity.id", o.VC.EntityID),
		attribute.String("biz.customer.id", o.VC.CustomerID),
		attribute.Int64("biz.amount_minor", o.VC.Money.Amount),
		attribute.String("biz.currency", o.VC.Money.Currency),
		attribute.Int("biz.exponent", int(o.VC.Money.Exponent)),
		attribute.String("biz.value.kind", string(o.VC.Kind)),
		attribute.Bool("biz.amount.est", o.VC.Estimated),
	}
	if o.VC.Segment != "" {
		kv = append(kv, attribute.String("biz.segment", o.VC.Segment))
	}
	if !o.VC.Deadline.IsZero() {
		kv = append(kv, attribute.String("biz.sla.deadline", o.VC.Deadline.UTC().Format("2006-01-02T15:04:05Z")))
	}
	if o.Source != "" {
		kv = append(kv, attribute.String("source", o.Source))
	}
	if o.Err != "" {
		kv = append(kv, attribute.String("error", o.Err))
	}
	return kv
}
