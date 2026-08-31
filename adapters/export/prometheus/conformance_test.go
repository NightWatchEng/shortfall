// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/testkit/conformance"
)

// promBackend reads how many series reached the registry — one per distinct
// label set, which is why the suite feeds distinct series (see
// conformance.sampleMetrics). Events is always 0: this exporter declares
// Events=false and the honesty invariant checks it delivers none.
type promBackend struct{ g prometheus.Gatherer }

func (b promBackend) MetricPoints() int {
	mfs, err := b.g.Gather()
	if err != nil {
		return -1 // force a conformance failure rather than hide the error
	}
	n := 0
	for _, mf := range mfs {
		n += len(mf.Metric)
	}
	return n
}
func (b promBackend) Events() int { return 0 }

type promHarness struct{}

func (promHarness) New() (emit.Exporter, conformance.Backend) {
	reg := prometheus.NewRegistry()
	e, err := New(WithRegisterer(reg, reg))
	if err != nil {
		panic(err) // a fresh registry cannot fail registration
	}
	return e, promBackend{g: reg}
}

func TestPrometheusConformance(t *testing.T) {
	conformance.RunExporter(t, promHarness{})
}
