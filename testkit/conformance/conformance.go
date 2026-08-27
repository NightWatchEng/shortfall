// Package conformance is the exporter conformance suite: the invariants
// EVERY emit.Exporter must honor, run against a concrete exporter through a
// harness the adapter provides. It lives in the core module and imports only
// emit + biz, so an adapter can call it from its own nested-module test
// without dragging the suite's dependencies — or the adapter's — across the
// boundary.
//
// The invariants (emit.Exporter's contract, ADR-0002):
//   - Flush on Shutdown with no loss: everything handed to a CAPABLE signal
//     before Shutdown reaches the backend after it. A buffering exporter
//     that drops on close fails here.
//   - Capability honesty: what Capabilities() declares matches what the
//     backend actually receives. An exporter that says Events=true must
//     deliver every event; one that says Events=false must deliver none
//     (it is honest about not doing events, not silently dropping them).
//   - Empty batches are a no-op: nil in, nothing out, no error.
package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

// Backend reads how many signals have actually reached a concrete
// exporter's backend. A harness implements it over its own wire form
// (OTLP metricdata, a Prometheus registry, StatsD lines) and reports
// counts, so the suite stays blind to format.
type Backend interface {
	// MetricPoints is the number of metric data points delivered so far.
	MetricPoints() int
	// Events is the number of outcome events delivered so far.
	Events() int
}

// Harness constructs a fresh exporter wired to a fresh in-memory backend.
// The suite calls New once per invariant so no state leaks between them.
type Harness interface {
	New() (emit.Exporter, Backend)
}

// Result is one invariant's outcome. Err is empty when the invariant held;
// Skipped marks an invariant that does not apply to this exporter's
// declared capabilities. Separating the pure result from the testing.T
// wrapper is what lets the suite test itself (see conformance_test.go).
type Result struct {
	Name    string
	Skipped bool
	Err     string
}

var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// sampleMetrics builds a fixed batch of metric points, each on a DISTINCT
// series (a unique currency label per point), stamped with its own time.
//
// Distinct series is load-bearing: an AGGREGATING backend (Prometheus,
// StatsD, a remote-write buffer) collapses points that share a label set
// into one series, so feeding n identical-label points would leave one
// series and make "delivered == n" untestable. n distinct series in must
// mean n series out — that invariant holds for a preserving exporter (OTLP,
// n data points) and an aggregating one alike, and a dropped point shows up
// as a missing series either way.
func sampleMetrics(n int) []emit.MetricPoint {
	currencies := []string{"USD", "EUR", "GBP", "JPY", "CHF", "CAD", "AUD", "SEK", "NZD", "NOK", "DKK", "SGD"}
	out := make([]emit.MetricPoint, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, emit.MetricPoint{
			Name: "biz_value_total",
			Labels: map[string]string{
				"flow": "invoice.pay", "stage": "capture", "outcome": "failed",
				"currency": currencies[i%len(currencies)], "kind": "fee", "segment": "smb",
			},
			Value: int64(100 + i),
			At:    baseTime.Add(time.Duration(i) * time.Second),
		})
	}
	return out
}

func sampleEvents(n int) []biz.Outcome {
	out := make([]biz.Outcome, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, biz.Outcome{
			At: baseTime.Add(time.Duration(i) * time.Second),
			VC: biz.ValueContext{
				Flow: "invoice.pay", EntityID: fmt.Sprintf("inv_%d", i), CustomerID: "h:c",
				Money: biz.Money{Amount: int64(100 + i), Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
			},
			Stage: "capture", Result: biz.ResultFailed,
		})
	}
	return out
}

// batchesOfMetrics feeds n points in three uneven Export calls so the
// invariant exercises batching, not a single shot. Returns total fed and
// the first Export error (nil if none).
func batchesOfMetrics(exp emit.Exporter, n int) (int, error) {
	pts := sampleMetrics(n)
	cuts := [][]emit.MetricPoint{pts[:n/2], pts[n/2 : n/2+n/4], pts[n/2+n/4:]}
	for _, b := range cuts {
		if len(b) == 0 {
			continue
		}
		if err := exp.ExportMetrics(context.Background(), b); err != nil {
			return n, err
		}
	}
	return n, nil
}

func batchesOfEvents(exp emit.Exporter, n int) (int, error) {
	evs := sampleEvents(n)
	cuts := [][]biz.Outcome{evs[:n/2], evs[n/2 : n/2+n/4], evs[n/2+n/4:]}
	for _, b := range cuts {
		if len(b) == 0 {
			continue
		}
		if err := exp.ExportEvents(context.Background(), b); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Check runs every invariant and returns one Result each (order stable).
// It is the pure core; RunExporter is the *testing.T wrapper. Kept
// exported so a harness author can assert conformance outside go test if
// they want, and so the suite can verify itself.
func Check(h Harness) []Result {
	probe, _ := h.New()
	caps := probe.Capabilities()
	_ = probe.Shutdown(context.Background())

	results := []Result{}

	if !caps.Metrics && !caps.Events {
		results = append(results, Result{
			Name: "declares at least one signal",
			Err:  "exporter declares neither Metrics nor Events — an exporter that ships nothing is a defect, not a capability",
		})
		return results
	}
	results = append(results, Result{Name: "declares at least one signal"})

	const n = 8

	// Flush-on-shutdown with no loss, per capable signal.
	results = append(results, metricNoLoss(h, caps, n))
	results = append(results, eventNoLoss(h, caps, n))

	// Capability honesty: an incapable signal delivers nothing.
	results = append(results, metricHonesty(h, caps, n))
	results = append(results, eventHonesty(h, caps, n))

	// Empty batches are a no-op.
	results = append(results, emptyNoop(h))

	return results
}

func metricNoLoss(h Harness, caps emit.Caps, n int) Result {
	r := Result{Name: "metrics flush on shutdown with no loss"}
	if !caps.Metrics {
		r.Skipped = true
		return r
	}
	exp, be := h.New()
	total, err := batchesOfMetrics(exp, n)
	if err != nil {
		r.Err = fmt.Sprintf("a capable exporter must not error on metric export: %v", err)
		return r
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		r.Err = fmt.Sprintf("shutdown errored: %v", err)
		return r
	}
	if be.MetricPoints() != total {
		r.Err = fmt.Sprintf("delivered %d of %d metric points after shutdown — batching or flush is dropping data", be.MetricPoints(), total)
	}
	return r
}

func eventNoLoss(h Harness, caps emit.Caps, n int) Result {
	r := Result{Name: "events flush on shutdown with no loss"}
	if !caps.Events {
		r.Skipped = true
		return r
	}
	exp, be := h.New()
	total, err := batchesOfEvents(exp, n)
	if err != nil {
		r.Err = fmt.Sprintf("a capable exporter must not error on event export: %v", err)
		return r
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		r.Err = fmt.Sprintf("shutdown errored: %v", err)
		return r
	}
	if be.Events() != total {
		r.Err = fmt.Sprintf("delivered %d of %d events after shutdown — an exporter that declares Events must not drop them", be.Events(), total)
	}
	return r
}

func metricHonesty(h Harness, caps emit.Caps, n int) Result {
	r := Result{Name: "metrics-incapable exporter delivers no metrics"}
	if caps.Metrics {
		r.Skipped = true
		return r
	}
	exp, be := h.New()
	// An incapable signal MAY return an error (signalling a drop) — that is
	// honest. What it must never do is silently deliver.
	_, _ = batchesOfMetrics(exp, n)
	_ = exp.Shutdown(context.Background())
	if be.MetricPoints() != 0 {
		r.Err = fmt.Sprintf("exporter declares Metrics=false but delivered %d points — capability dishonesty", be.MetricPoints())
	}
	return r
}

func eventHonesty(h Harness, caps emit.Caps, n int) Result {
	r := Result{Name: "events-incapable exporter delivers no events"}
	if caps.Events {
		r.Skipped = true
		return r
	}
	exp, be := h.New()
	_, _ = batchesOfEvents(exp, n)
	_ = exp.Shutdown(context.Background())
	if be.Events() != 0 {
		r.Err = fmt.Sprintf("exporter declares Events=false but delivered %d events — capability dishonesty", be.Events())
	}
	return r
}

func emptyNoop(h Harness) Result {
	r := Result{Name: "empty batches never reach the backend"}
	exp, be := h.New()
	if err := exp.ExportMetrics(context.Background(), nil); err != nil {
		r.Err = fmt.Sprintf("empty metric export errored: %v", err)
		return r
	}
	if err := exp.ExportEvents(context.Background(), nil); err != nil {
		r.Err = fmt.Sprintf("empty event export errored: %v", err)
		return r
	}
	_ = exp.Shutdown(context.Background())
	if be.MetricPoints() != 0 || be.Events() != 0 {
		r.Err = fmt.Sprintf("empty batches delivered %d metrics / %d events — must be a no-op", be.MetricPoints(), be.Events())
	}
	return r
}

// RunExporter runs the conformance suite against a concrete exporter as
// subtests. Every adapters/export/* module MUST call this from a test —
// enforcement_test.go fails the build of any exporter module that does not.
func RunExporter(t *testing.T, h Harness) {
	t.Helper()
	for _, res := range Check(h) {
		res := res
		t.Run(res.Name, func(t *testing.T) {
			if res.Skipped {
				t.Skip("not applicable to this exporter's declared capabilities")
			}
			if res.Err != "" {
				t.Fatal(res.Err)
			}
		})
	}
}
