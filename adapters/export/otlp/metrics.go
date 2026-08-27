package otlp

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/NightWatchEng/shortfall/emit"
)

// scope names the emitting library on every exported metric/log.
var scope = instrumentation.Scope{
	Name:    "github.com/NightWatchEng/shortfall",
	Version: "v0.1.0",
}

// gaugeFamilies are the metric names that map to a Gauge (a level);
// everything else is a monotonic delta Sum (a counter).
var gaugeFamilies = map[string]bool{
	"biz_inflight_value": true,
}

// buildResourceMetrics groups a MetricPoint batch by family into OTLP
// metric data. Counter families become delta monotonic Sums; gauge
// families become Gauges. Each point keeps its own observation time — a
// batch delayed by an incident must not restamp money to "now".
func buildResourceMetrics(batch []emit.MetricPoint) *metricdata.ResourceMetrics {
	byName := map[string][]metricdata.DataPoint[int64]{}
	order := make([]string, 0)
	for _, p := range batch {
		if _, seen := byName[p.Name]; !seen {
			order = append(order, p.Name)
		}
		byName[p.Name] = append(byName[p.Name], metricdata.DataPoint[int64]{
			Attributes: attrsOf(p.Labels),
			Time:       p.At,
			Value:      p.Value,
		})
	}

	metrics := make([]metricdata.Metrics, 0, len(order))
	for _, name := range order {
		pts := byName[name]
		var data metricdata.Aggregation
		if gaugeFamilies[name] {
			data = metricdata.Gauge[int64]{DataPoints: pts}
		} else {
			data = metricdata.Sum[int64]{
				DataPoints:  pts,
				Temporality: metricdata.DeltaTemporality,
				IsMonotonic: true,
			}
		}
		metrics = append(metrics, metricdata.Metrics{Name: name, Data: data})
	}

	return &metricdata.ResourceMetrics{
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Scope:   scope,
			Metrics: metrics,
		}},
	}
}

// attrsOf turns a bounded label map into an OTLP attribute set. Labels
// are the fixed ADR-0004 families only — the emit layer guarantees that,
// so no cardinality surprise crosses here.
func attrsOf(labels map[string]string) attribute.Set {
	kvs := make([]attribute.KeyValue, 0, len(labels))
	for k, v := range labels {
		kvs = append(kvs, attribute.String(k, v))
	}
	return attribute.NewSet(kvs...)
}
