// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

//go:build benchload

// Load and concurrency benchmarks that are deliberately kept out of the PR
// gate. They are measurements to read, not baselines to hold.
//
// scripts/ci-bench.sh discovers benchmarks with `go test -list` and runs
// every one it finds at BENCH_TIME=1x, BENCH_COUNT=6 on each pull request,
// comparing against main with benchstat. Nothing here belongs in that
// comparison, for two separate reasons:
//
//   - Every benchmark in this file uses b.RunParallel. At -benchtime 1x,
//     b.N is 1, so RunParallel spawns GOMAXPROCS goroutines to run a single
//     iteration and the result is scheduler noise. Measured over two runs
//     of an identical tree at the CI settings: ±350%+ spreads and a
//     benchstat "32% regression" at p=0.041 against no change at all.
//   - The backend-pathology benchmarks additionally spend most of their
//     time waiting on a 25 ms exporter or on a starved lock.
//
// The concurrency CURVES these produce are real and published — they come
// from an explicit -cpu sweep at -benchtime 1s, where b.N is large enough
// for RunParallel to measure the work rather than the goroutine spawn:
//
//	go test -tags benchload -run '^$' \
//	    -bench 'RecordParallel$|RecordParallelSuppressed$|TrackerTrackDoneParallel$|ParallelSeqFloor$' \
//	    -benchmem -benchtime 1s -count 6 -cpu 1,2,4,8,18 ./emit
//
//	go test -tags benchload -run '^$' \
//	    -bench 'HealthyBackend|SlowExporter|FailingExporter|UnderPublish' \
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

	"github.com/NightWatchEng/shortfall/biz"
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

// backendBuffer is the event-buffer bound every backend-pathology
// benchmark runs with, the healthy control included. It is small on
// purpose — a wedged backend has to actually fill it inside a benchmark
// window. The published drop percentages are a function of this number as
// much as of the backend, which is why it is one named constant quoted in
// docs/performance.md rather than a literal at three call sites.
const backendBuffer = 1 << 14

// BenchmarkRecordHealthyBackend is the control for the two pathology
// benchmarks below: same buffer, same flush cadence, same parallel accept
// path, and a sink that answers instantly. Without it the slow-backend row
// gets compared against BenchmarkRecordParallel, which runs a 64x larger
// buffer and a 2x faster flusher — a difference that produces the drop
// contrast on its own and has nothing to do with the backend. Review
// caught exactly that: with this buffer held constant, the "wedged
// backend" scenario is a real comparison rather than a harness artefact.
func BenchmarkRecordHealthyBackend(b *testing.B) {
	recordUnderBackend(b, newLoadExporter(), backendBuffer)
}

// BenchmarkRecordSlowExporter answers whether a slow backend reaches the
// caller. It does not: Record's ns/op stays in the band
// BenchmarkRecordHealthyBackend reports while the backend takes 25 ms per
// event batch. The price is paid in %dropped instead — the bounded buffer
// fills because the flusher is stuck in the exporter, and Record sheds
// load.
func BenchmarkRecordSlowExporter(b *testing.B) {
	exp := newLoadExporter()
	exp.delay = backendLatency
	recordUnderBackend(b, exp, backendBuffer)
}

// BenchmarkRecordFailingExporter is the other backend pathology: fast
// refusal rather than slow acceptance. Batches are handed over and rejected
// immediately, so the buffer keeps draining and Record keeps accepting —
// the loss shows up as reason=export rather than reason=overflow, and the
// caller pays nothing extra.
func BenchmarkRecordFailingExporter(b *testing.B) {
	exp := newLoadExporter()
	exp.fail.Store(true)
	recordUnderBackend(b, exp, backendBuffer)
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

func (l *loadExporter) dropTotal() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, v := range l.drops {
		n += v
	}
	return n
}

// BenchmarkRecordParallel is the scaling question: does Record get faster
// with cores, or does every caller queue behind the emitter's single mutex?
// Sweep it with -cpu 1,2,4,8,18 to read the curve.
//
// A background flusher drains to a discard sink on a 500us cadence, which
// is how the library is actually wired — a Record benchmark with no flusher
// would measure an emitter no production service runs.
func BenchmarkRecordParallel(b *testing.B) {
	exp := newLoadExporter()
	em := newLoadEmitter(b, exp,
		WithBufferSize(1<<20),
		WithFlushInterval(500*time.Microsecond),
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

	if err := em.Close(context.Background()); err != nil {
		b.Fatalf("close: %v", err)
	}
	// Conservation: every call took the accept path and every accepted
	// outcome reached the sink. A shortfall here means the run silently
	// measured suppression or overflow instead of acceptance, which would
	// make the reported ns/op describe a path nobody asked about.
	calls := seq.Load()
	if calls != int64(b.N) {
		b.Fatalf("issued %d Record calls, b.N = %d", calls, b.N)
	}
	if got := exp.events.Load(); got != calls {
		b.Fatalf("exported %d events for %d accepted Record calls (drops=%d pending=%d): "+
			"the benchmark stopped measuring the accept path",
			got, calls, exp.dropTotal(), pendingDrops(em))
	}
}

// BenchmarkParallelSeqFloor is the harness floor for
// BenchmarkRecordParallel: the same RunParallel loop with the same shared
// sequence counter and nothing else in it.
//
// BenchmarkRecordParallel needs one contended atomic increment per call to
// mint a distinct de-dup key, and a contended cache line is not free — at
// high -cpu values that increment is a measurable share of a
// sub-microsecond operation. Subtract this row from that table to read
// Record's own cost. It is a committed benchmark rather than a note in the
// docs because a correction nobody can reproduce is not a correction.
//
// The other parallel benchmarks here count iterations in a goroutine-local
// variable and publish the total once, so this floor does not apply to
// them.
func BenchmarkParallelSeqFloor(b *testing.B) {
	var seq atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = seq.Add(1)
		}
	})
	b.StopTimer()
	if seq.Load() != int64(b.N) {
		b.Fatalf("counted %d increments, b.N = %d", seq.Load(), b.N)
	}
}

// BenchmarkRecordParallelSuppressed is the same contention with none of the
// work: every goroutine re-records one already-seen key, so each call takes
// the emitter lock, hits the de-dup set, and returns. The gap between this
// and BenchmarkRecordParallel is the cost of the accepted path; the shape
// of this curve alone is the cost of the lock.
func BenchmarkRecordParallelSuppressed(b *testing.B) {
	exp := newLoadExporter()
	em := newLoadEmitter(b, exp, WithFlushInterval(0))
	ctxs := benchContexts(b, 1)
	var calls atomic.Int64

	em.Record(ctxs[0], "capture", biz.ResultFailed) // prime the de-dup set

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Counted locally and published once. A shared atomic incremented
		// inside the loop is itself a contended cache line, and on a
		// sub-microsecond operation it would be a meaningful share of what
		// this benchmark reports as the emitter's lock cost.
		var local int64
		for pb.Next() {
			em.Record(ctxs[0], "capture", biz.ResultFailed)
			local++
		}
		calls.Add(local)
	})
	b.StopTimer()

	if err := em.Close(context.Background()); err != nil {
		b.Fatalf("close: %v", err)
	}
	if calls.Load() != int64(b.N) {
		b.Fatalf("issued %d Record calls, b.N = %d", calls.Load(), b.N)
	}
	// Exactly the priming outcome reaches the sink: anything more means a
	// call was accepted and this stopped being the suppression path.
	if got := exp.events.Load(); got != 1 {
		b.Fatalf("exported %d events, want exactly the primed 1 — calls were not suppressed", got)
	}
}

// trackerIDs builds one private id block per goroutine slot so paired
// Track/Done calls never collide across goroutines: a collision would let
// one goroutine's Done remove another's entry and the drained-to-empty
// check at the end would stop meaning anything.
func trackerIDs(slots, perSlot int) [][]string {
	ids := make([][]string, slots)
	for s := range ids {
		block := make([]string, perSlot)
		for i := range block {
			block[i] = fmt.Sprintf("m%03d_%04d", s, i)
		}
		ids[s] = block
	}
	return ids
}

const trackerSlots, trackerPerSlot = 256, 64

// BenchmarkTrackerTrackDoneParallel is the queue-consumer hot path under
// contention: Track on receive, Done on completion, from every consumer
// goroutine at once against one shared tracker. The in-flight set is
// sharded by id hash, so this benchmark is where a contention
// regression on the shard locks would show up. Sweep with
// -cpu 1,2,4,8,18.
func BenchmarkTrackerTrackDoneParallel(b *testing.B) {
	ce := &countingEmitter{}
	tr := NewInFlightTracker(ce, WithTrackerLogger(quietLogger()))
	ids := trackerIDs(trackerSlots, trackerPerSlot)
	now := time.Now()
	var slot, calls atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// One atomic to claim an id block, one to publish the count, and
		// none in between: a shared counter incremented every iteration
		// would contend on its own cache line and this benchmark would end
		// up reporting that contention as the tracker's.
		block := ids[int(slot.Add(1)-1)%len(ids)]
		var local int64
		for pb.Next() {
			id := block[local%int64(len(block))]
			tr.Track("invoice.pay", "capture", id, usd(1499), now)
			tr.Done("invoice.pay", "capture", id)
			local++
		}
		calls.Add(local)
	})
	b.StopTimer()

	if calls.Load() != int64(b.N) {
		b.Fatalf("issued %d Track/Done pairs, b.N = %d", calls.Load(), b.N)
	}
	if n := trackerItemCount(tr); n != 0 {
		b.Fatalf("%d items left in flight after %d paired Track/Done calls", n, calls.Load())
	}
	if tr.Overflowed() != 0 || tr.Rejected() != 0 {
		b.Fatalf("tracker overflowed=%d rejected=%d: the measured path was not the accept path",
			tr.Overflowed(), tr.Rejected())
	}
}
