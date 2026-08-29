//go:build benchload

// Load benchmarks that are deliberately kept out of the PR gate.
//
// scripts/ci-bench.sh discovers benchmarks with `go test -list` and runs
// them at count=6 on every pull request, comparing against main with
// benchstat. The three below do not belong in that comparison: each spends
// most of its time waiting on a pathological backend or on a starved lock,
// so their variance is large enough to manufacture regressions on PRs that
// changed nothing. They are measurements to read, not baselines to hold.
//
// Run them explicitly:
//
//	go test -tags benchload -run '^$' -bench 'SlowExporter|FailingExporter|UnderPublish' \
//	    -benchmem -benchtime 2s -count 6 ./emit
//
// The build tag is why ci-bench.sh never sees them, and docs/performance.md
// says so where the numbers are published. The cost of the tag is that
// nothing in CI compiles this file — see the limits section of that doc.

package emit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// backendLatency is the per-batch stall the slow-backend benchmark models:
// a collector that has gone from microseconds to tens of milliseconds, the
// shape of a degraded ingest endpoint rather than a dead one.
const backendLatency = 25 * time.Millisecond

// recordUnderBackend runs Record in parallel against a sink configured by
// the caller and returns (calls, exported, dropped). It reports the drop
// percentage as a benchmark metric because ns/op alone is misleading here:
// the overflow path is CHEAPER than the accept path, so a Record benchmark
// against a wedged backend gets faster as it loses more data. The drop
// percentage is the number that says what the latency bought.
func recordUnderBackend(b *testing.B, exp *loadExporter, bufSize int) {
	b.Helper()
	em := newLoadEmitter(b, exp,
		WithBufferSize(bufSize),
		WithFlushInterval(time.Millisecond),
	)
	ctxs := benchContextsFor(b, b.N)
	var seq atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recordAt(em, ctxs, seq.Add(1)-1)
		}
	})
	b.StopTimer()

	// A backend that answers again for the shutdown flush, so the counters
	// this benchmark reports can actually reach the sink. fail is atomic
	// precisely because the background flusher may still be inside an
	// export when it is cleared; delay is left alone for the same reason —
	// it is written once at setup and never again.
	exp.fail.Store(false)
	if err := em.Close(context.Background()); err != nil {
		b.Fatalf("close: %v", err)
	}

	calls := seq.Load()
	if calls != int64(b.N) {
		b.Fatalf("issued %d Record calls, b.N = %d", calls, b.N)
	}
	exported, dropped := exp.events.Load(), exp.dropTotal()+pendingDrops(em)
	// Conservation: every call is either an exported outcome or a counted
	// drop. Silent loss under backend pressure is the exact defect the
	// review charter names, and a benchmark that let it through would be
	// measuring the wrong emitter.
	if exported+dropped != calls {
		b.Fatalf("%d calls accounted for as %d exported + %d dropped — %d unaccounted",
			calls, exported, dropped, calls-exported-dropped)
	}
	b.ReportMetric(100*float64(dropped)/float64(calls), "%dropped")
	b.ReportMetric(float64(exp.batches.Load()), "batches")
}

// BenchmarkRecordSlowExporter answers whether a slow backend reaches the
// caller. It does not: Record's ns/op stays in the same band as
// BenchmarkRecordParallel while the backend takes 25 ms a batch. The price
// is paid in %dropped instead — the bounded buffer fills because the
// flusher is stuck in the exporter, and Record sheds load.
func BenchmarkRecordSlowExporter(b *testing.B) {
	exp := newLoadExporter()
	exp.delay = backendLatency
	recordUnderBackend(b, exp, 1<<14)
}

// BenchmarkRecordFailingExporter is the other backend pathology: fast
// refusal rather than slow acceptance. Batches are handed over and rejected
// immediately, so the buffer keeps draining and Record keeps accepting —
// the loss shows up as reason=export rather than reason=overflow, and the
// caller pays nothing extra.
func BenchmarkRecordFailingExporter(b *testing.B) {
	exp := newLoadExporter()
	exp.fail.Store(true)
	recordUnderBackend(b, exp, 1<<14)
}

// BenchmarkTrackerTrackDoneUnderPublish is the worst case for the tracker's
// single mutex: a publisher that is always inside its critical section,
// against consumers trying to Track and Done. It is an upper bound on
// interference, not a production cadence — a real deployment publishes on
// an interval measured in seconds, and BenchmarkTrackerPublishScale gives
// the per-publish stall to multiply by that cadence.
//
// The publisher is a spin loop rather than a ticker on purpose: a ticker
// would make the measurement depend on whether a tick landed inside the
// timed region, and at -benchtime 1x it usually would not. Here the first
// publish is awaited on a channel before the timer starts, so the
// contention is present for every configuration.
func BenchmarkTrackerTrackDoneUnderPublish(b *testing.B) {
	const seeded = 10_000

	ce := &countingEmitter{}
	tr := NewInFlightTracker(ce, WithTrackerLogger(quietLogger()))
	now := time.Now()
	for i := 0; i < seeded; i++ {
		tr.Track("invoice.pay", "settle", fmt.Sprintf("seed%07d", i), usd(1499),
			now.Add(-time.Duration(i)*time.Second))
	}

	stop := make(chan struct{})
	first := make(chan struct{})
	var firstOnce sync.Once
	var publishes atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tr.Publish()
			publishes.Add(1)
			firstOnce.Do(func() { close(first) })
		}
	}()
	<-first // contention is established before anything is timed

	ids := trackerIDs(trackerSlots, trackerPerSlot)
	var slot, calls atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		block := ids[int(slot.Add(1)-1)%len(ids)]
		var local int64
		for pb.Next() {
			id := block[local%int64(len(block))]
			tr.Track("invoice.pay", "capture", id, usd(1499), now)
			tr.Done("invoice.pay", "capture", id)
			local++
		}
		calls.Add(local) // once per goroutine, never inside the loop
	})
	b.StopTimer()
	close(stop)
	<-done

	if calls.Load() != int64(b.N) {
		b.Fatalf("issued %d Track/Done pairs, b.N = %d", calls.Load(), b.N)
	}
	if publishes.Load() < 1 {
		b.Fatal("no publish completed: the contention this benchmark exists to measure was absent")
	}
	if n := trackerItemCount(tr); n != seeded {
		b.Fatalf("%d items in flight, want the %d seeded — every benchmark Track was paired with a Done", n, seeded)
	}
	b.ReportMetric(float64(publishes.Load()), "publishes")
}
