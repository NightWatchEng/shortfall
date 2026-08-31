// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package otlp

import (
	"fmt"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"

	"github.com/NightWatchEng/shortfall/emit"
)

// scope names the emitting library on every exported metric/log.
var scope = instrumentation.Scope{
	Name:    "github.com/NightWatchEng/shortfall",
	Version: "v0.1.0",
}

// gaugeFamilies are the metric names that map to a Gauge (a level).
var gaugeFamilies = map[string]bool{
	"biz_inflight_value": true,
	"biz_inflight_count": true,
}

// counterFamilies are the metric names that map to a monotonic delta Sum.
var counterFamilies = map[string]bool{
	"biz_value_total":          true,
	"biz_txn_total":            true,
	"biz_provider_calls_total": true,
	"biz_dropped_events_total": true,
}

// unknownFamilyError names a metric family this exporter does not recognise.
// The alternative — treating anything unrecognised as a counter — would ship
// a mistyped or newly added level family as a monotonic Sum, which a backend
// then adds up: silently wrong arithmetic on money rather than a loud stop.
type unknownFamilyError struct{ name string }

func (e *unknownFamilyError) Error() string {
	return fmt.Sprintf("otlp: unknown metric family %q", e.name)
}

// buildResourceMetrics groups a MetricPoint batch by family into OTLP
// metric data. Counter families become delta monotonic Sums; gauge
// families become Gauges. Each point keeps its own observation time — a
// batch delayed by an incident must not restamp money to "now".
func buildResourceMetrics(batch []emit.MetricPoint, res *resource.Resource) (*metricdata.ResourceMetrics, error) {
	byName := map[string][]metricdata.DataPoint[int64]{}
	order := make([]string, 0)
	for _, p := range batch {
		if !gaugeFamilies[p.Name] && !counterFamilies[p.Name] {
			return nil, &unknownFamilyError{name: p.Name}
		}

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
		Resource: res,
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Scope:   scope,
			Metrics: metrics,
		}},
	}, nil
}

// defaultResource identifies this process as the writer of its series. The
// ADR-0004 label sets carry no writer identity, so without a per-process
// service.instance.id every replica publishes one shared gauge series for the
// in-flight families — and a gauge is a level, so the backend keeps one
// replica's value and the deferred leg under-reports by roughly the replica
// count. Delta sums are unaffected (a backend adds them across writers); the
// gauges are why this matters.
func defaultResource() *resource.Resource {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("shortfall"),
		semconv.ServiceInstanceID(fmt.Sprintf("%s-%d", host, os.Getpid())),
	)
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
