package cloudwatch

import (
	"encoding/json"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// EMF is the CloudWatch Embedded Metric Format: a JSON log record whose
// `_aws` block tells CloudWatch which top-level fields are metrics and what
// their dimensions are. Writing these records to a log stream (stdout under
// the CloudWatch agent, or PutLogEvents) gets metrics extracted for free,
// and — unlike Prometheus text — each record carries its own millisecond
// Timestamp, so a batch delayed by an incident keeps money at the outcome's
// time, not ingestion time.
//
// See https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format_Specification.html

// defaultNamespace is the CloudWatch namespace for shortfall metrics.
const defaultNamespace = "shortfall"

// Fixed per-family dimension sets (ADR-0004). Order is the contract.
var (
	valueDims    = []string{"flow", "stage", "outcome", "currency", "kind", "segment"}
	txnDims      = []string{"flow", "stage", "outcome", "currency", "segment"}
	inflightDims = []string{"flow", "stage", "age_bucket", "currency"}
	providerDims = []string{"provider", "op", "outcome"}
	droppedDims  = []string{"reason"}
)

// dimsFor returns the fixed dimension set for a metric family, or nil for an
// unknown family (the caller turns that into a visible error).
func dimsFor(name string) []string {
	switch name {
	case "biz_value_total":
		return valueDims
	case "biz_txn_total":
		return txnDims
	case "biz_inflight_value", "biz_inflight_count":
		return inflightDims
	case "biz_provider_calls_total":
		return providerDims
	case "biz_dropped_events_total":
		return droppedDims
	default:
		return nil
	}
}

// emfMetric names one metric within an _aws block.
type emfMetric struct {
	Name string `json:"Name"`
	Unit string `json:"Unit,omitempty"`
}

// emfDirective is one Namespace/Dimensions/Metrics grouping.
type emfDirective struct {
	Namespace  string      `json:"Namespace"`
	Dimensions [][]string  `json:"Dimensions"`
	Metrics    []emfMetric `json:"Metrics"`
}

// emfMeta is the `_aws` block.
type emfMeta struct {
	Timestamp         int64          `json:"Timestamp"`
	CloudWatchMetrics []emfDirective `json:"CloudWatchMetrics"`
}

// buildMetricRecord renders one MetricPoint as an EMF record: the value is a
// metric under its ADR-0004 dimensions, each dimension is also a top-level
// field (EMF requires the dimension values to appear as members), and the
// record's Timestamp is the point's own observation time.
//
// The metric value carries the amount/count — ADR-0004 forbids amounts as
// dimensions — and dimsFor pins the dimension set so no unbounded key can
// become a dimension.
func buildMetricRecord(namespace, unit string, p emit.MetricPoint) ([]byte, error) {
	dims := dimsFor(p.Name)
	if dims == nil {
		return nil, &unknownFamilyError{name: p.Name}
	}
	rec := map[string]any{
		"_aws": emfMeta{
			Timestamp: p.At.UnixMilli(),
			CloudWatchMetrics: []emfDirective{{
				Namespace:  namespace,
				Dimensions: [][]string{dims},
				Metrics:    []emfMetric{{Name: p.Name, Unit: unit}},
			}},
		},
		p.Name: p.Value,
	}
	for _, d := range dims {
		rec[d] = p.Labels[d]
	}
	return json.Marshal(rec)
}

// buildEventRecord renders one Outcome as a structured EMF-style log record.
// It carries the individual amount and ids (events-only data, ADR-0004) as
// fields for CloudWatch Logs Insights — it declares no metric, so it never
// double-counts the aggregated families the metric path already emits. Its
// Timestamp is the outcome's own time.
func buildEventRecord(o biz.Outcome) ([]byte, error) {
	rec := map[string]any{
		"_aws":              map[string]any{"Timestamp": o.At.UnixMilli()},
		biz.EventKey:        biz.EventOutcome,
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
	// See the note in the GCP exporter: the deadline rides every transport
	// that can express it, so the same Outcome does not produce different
	// fields depending on which exporter is wired (ADR-0002).
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
	}
	return json.Marshal(rec)
}
