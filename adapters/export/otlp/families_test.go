package otlp

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/NightWatchEng/shortfall/emit"
)

// TestEveryFamilyMapsToItsKind covers all six ADR-0004 families, not the
// three the kinds test samples: a family the adapter forgets to classify
// would silently ship as a counter and read as a rate on a dashboard.
func TestEveryFamilyMapsToItsKind(t *testing.T) {
	cases := []struct {
		family   string
		wantKind string
	}{
		{"biz_value_total", "sum"},
		{"biz_txn_total", "sum"},
		{"biz_provider_calls_total", "sum"},
		{"biz_dropped_events_total", "sum"},
		{"biz_inflight_value", "gauge"},
		{"biz_inflight_count", "gauge"},
	}
	for _, c := range cases {
		t.Run(c.family, func(t *testing.T) {
			rm, err := buildResourceMetrics([]emit.MetricPoint{
				{Name: c.family, Labels: map[string]string{"flow": "invoice.pay"}, Value: 7, At: at},
			}, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			metrics := rm.ScopeMetrics[0].Metrics
			if len(metrics) != 1 {
				t.Fatalf("got %d metrics, want 1", len(metrics))
			}
			switch d := metrics[0].Data.(type) {
			case metricdata.Sum[int64]:
				if c.wantKind != "sum" {
					t.Fatalf("%s mapped to a sum, want %s", c.family, c.wantKind)
				}
				if d.Temporality != metricdata.DeltaTemporality {
					t.Errorf("%s temporality = %v, want delta — emit hands out deltas, not running totals", c.family, d.Temporality)
				}
				if !d.IsMonotonic {
					t.Errorf("%s must be monotonic", c.family)
				}
			case metricdata.Gauge[int64]:
				if c.wantKind != "gauge" {
					t.Fatalf("%s mapped to a gauge, want %s", c.family, c.wantKind)
				}
			default:
				t.Fatalf("%s mapped to %T, which is neither an int64 sum nor an int64 gauge", c.family, d)
			}
		})
	}
}

// TestAmountsNeverUseAFloatInstrument pins the money representation on the
// wire. OTel's data model offers float64 aggregations, and money routed
// through one would round silently above 2^53 minor units — the exact drift
// int64 minor units exist to prevent (ADR-0001).
func TestAmountsNeverUseAFloatInstrument(t *testing.T) {
	cases := []struct {
		name  string
		value int64
	}{
		{"ordinary amount", 14900},
		{"beyond exact float64 integers", 1 << 54},
		{"max int64", 1<<63 - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rm, err := buildResourceMetrics([]emit.MetricPoint{
				{Name: "biz_value_total", Labels: map[string]string{"currency": "USD"}, Value: c.value, At: at},
				{Name: "biz_inflight_value", Labels: map[string]string{"currency": "USD"}, Value: c.value, At: at},
			}, nil)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for _, m := range rm.ScopeMetrics[0].Metrics {
				switch d := m.Data.(type) {
				case metricdata.Sum[float64], metricdata.Gauge[float64]:
					t.Fatalf("%s used a float64 aggregation — money must never pass through one", m.Name)
				case metricdata.Sum[int64]:
					if got := d.DataPoints[0].Value; got != c.value {
						t.Errorf("%s value = %d, want %d", m.Name, got, c.value)
					}
				case metricdata.Gauge[int64]:
					if got := d.DataPoints[0].Value; got != c.value {
						t.Errorf("%s value = %d, want %d", m.Name, got, c.value)
					}
				default:
					t.Fatalf("%s used an unexpected aggregation %T", m.Name, d)
				}
			}
		})
	}
}

// TestPointsKeepTheirObservationTime pins that a batch delayed by an
// incident does not restamp money to flush time — the window an outcome
// falls in decides which incident it is counted against.
func TestPointsKeepTheirObservationTime(t *testing.T) {
	earlier := at.Add(-90 * time.Minute)
	rm, err := buildResourceMetrics([]emit.MetricPoint{
		{Name: "biz_txn_total", Labels: map[string]string{"flow": "f"}, Value: 1, At: earlier},
		{Name: "biz_txn_total", Labels: map[string]string{"flow": "f"}, Value: 1, At: at},
	}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sum, ok := rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("biz_txn_total is not an int64 sum")
	}
	if len(sum.DataPoints) != 2 {
		t.Fatalf("got %d points, want 2", len(sum.DataPoints))
	}
	if !sum.DataPoints[0].Time.Equal(earlier) || !sum.DataPoints[1].Time.Equal(at) {
		t.Errorf("points restamped: got %v and %v, want %v and %v",
			sum.DataPoints[0].Time, sum.DataPoints[1].Time, earlier, at)
	}
}
