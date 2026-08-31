// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

//go:build benchload

// The engine.Compute scaling series, deliberately kept out of the PR gate.
//
// scripts/ci-bench.sh discovers benchmarks with `go test -list` and runs
// every one it finds at count=6 on each pull request. This series takes
// about 42 s at count=6, and the gate would run it twice — once on the PR
// head and once on main — which is why it is out: wall clock, not memory.
// The build tag is how it stays out — `go test -list` never sees a file it
// does not build — and docs/performance.md says so beside the numbers.
//
// The gate keeps BenchmarkCompute (50k and 200k events, in
// compute_bench_test.go) as the PR-vs-main comparison. This series exists
// to answer a different question: what SHAPE does Compute have, so a reader
// can price a window they have not measured.
//
// Run it explicitly:
//
//	go test -tags benchload -run '^$' -bench ComputeScale -benchmem \
//	    -benchtime 1x -count 6 ./engine
//
// Peak resident memory at the 2M step is about 2.7 GB, against a live heap
// peaking near 1.06 GB; the 4.35 GB in the B/op column is cumulative
// allocation over the call, not residency. The cost of the tag
// is that nothing in CI compiles this file — see the limits section of
// docs/performance.md.

package engine

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/NightWatchEng/shortfall/registry"
)

// computeScaleSizes spans about 2.3 orders of magnitude, straddling the
// gate's 50k and 200k points. 2M is included rather than extrapolated: the whole
// reason to publish a shape is that a reader should not have to guess
// whether the top of the range is linear, and a guess published as a
// measurement would be the dishonesty ADR-0008 forbids.
var computeScaleSizes = []int{10_000, 100_000, 1_000_000, 2_000_000}

// BenchmarkComputeScale measures the full four-leg assembly across event
// counts, reusing buildIncident and benchWindow from the gate benchmark so
// the two series describe the same dataset shape and compose.
func BenchmarkComputeScale(b *testing.B) {
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		b.Fatal(err)
	}
	req := Request{Window: benchWindow, Flows: []string{"invoice.pay"}}
	for _, n := range computeScaleSizes {
		q := buildIncident(n)
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				rep, err := Compute(context.Background(), &reg, q, req)
				if err != nil {
					b.Fatal(err)
				}
				if len(rep.Realized.ByCurrency) == 0 {
					b.Fatal("benchmark computed an empty realized leg — dataset wrong")
				}
				if rep.Customers.Distinct == 0 {
					b.Fatal("benchmark computed an empty customers leg — dataset wrong")
				}
			}
		})
		// Drop the dataset before building the next, larger one: holding
		// two of these at once would roughly double the peak, and the 2M
		// step already carries the largest dataset in the series.
		q = nil
		runtime.GC()
	}
}
