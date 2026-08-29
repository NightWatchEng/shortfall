package emit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

// Concurrency and back-pressure coverage for the two hot paths an adopting
// service puts in its request and queue-consumer loops: emit.Record and
// emit.InFlightTracker. The single-goroutine benchmarks next door
// (BenchmarkRecordAccept, BenchmarkTrackerPublish10k) say what one caller
// pays; these say what happens when a payments service runs eighteen of
// them at once, and what a slow backend does to the caller.
//
// The goroutine curve comes from -cpu, not from hand-rolled goroutine
// plumbing: `go test -bench 'Parallel' -cpu 1,2,4,8,18` varies GOMAXPROCS
// and RunParallel's goroutine count together, benchstat folds the -N
// suffixes into one comparison, and no assertion anywhere depends on one
// goroutine outracing another. Reproduce commands live in
// docs/performance.md.
//
// Heavier and inherently noisier load benchmarks (slow backend, erroring
// backend, tracker starvation) sit in concurrency_load_bench_test.go behind
// the `benchload` build tag so scripts/ci-bench.sh never discovers them.

// quietLogger silences the emitter's warning stream. Benchmarks that
// deliberately overflow a buffer would otherwise spend their time in slog,
// and the drop accounting is asserted from counters, not from log lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// loadExporter is the sink for load work: it counts what it is handed and
// keeps none of it, so a multi-million-call run measures Record rather than
// the garbage collector. Its switches model the two backend pathologies the
// emitter promises to survive — latency (delay) and failure (fail) — plus a
// backend that has stopped answering entirely (release), which is what the
// deterministic back-pressure test needs.
//
// delay and the release gate apply to the event path only. Events are the
// path that carries outcomes and the one Flush exports first, so one
// addressable blocking point there is enough to hold a whole flush open —
// and gating both paths would leave the test with two things to wait on
// instead of one.
type loadExporter struct {
	delay time.Duration
	fail  atomic.Bool

	// entered closes when the first event export reaches the gate;
	// release is closed by the test to let exports through. Both nil
	// means an ungated exporter.
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once

	events  atomic.Int64
	batches atomic.Int64

	mu    sync.Mutex
	drops map[string]int64
}

func newLoadExporter() *loadExporter {
	return &loadExporter{drops: map[string]int64{}}
}

// gated returns an exporter whose first event export blocks until release
// is closed, with entered closed once it is in there — the test's proof
// that the flusher is inside the backend and the emitter's buffer is empty,
// established by a channel receive rather than a sleep.
func newGatedLoadExporter() *loadExporter {
	l := newLoadExporter()
	l.entered = make(chan struct{})
	l.release = make(chan struct{})
	return l
}

func (l *loadExporter) hold() {
	if l.entered != nil {
		l.enterOnce.Do(func() { close(l.entered) })
	}
	if l.release != nil {
		<-l.release
	}
	if l.delay > 0 {
		time.Sleep(l.delay)
	}
}

func (l *loadExporter) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	l.batches.Add(1)
	l.hold()
	// The failure check follows the hold so a gated exporter can be
	// switched to failing while it is held.
	if l.fail.Load() {
		return errors.New("backend down")
	}
	l.events.Add(int64(len(batch)))
	return nil
}

func (l *loadExporter) ExportMetrics(_ context.Context, batch []MetricPoint) error {
	if l.delay > 0 {
		time.Sleep(l.delay)
	}
	if l.fail.Load() {
		// Returning before counting is deliberate: Flush re-credits the
		// biz_dropped_events_total points of a failed metric batch back
		// into the emitter, and counting them here as well would book the
		// same drop twice.
		return errors.New("backend down")
	}
	l.mu.Lock()
	for _, p := range batch {
		if p.Name == "biz_dropped_events_total" {
			l.drops[p.Labels["reason"]] += p.Value
		}
	}
	l.mu.Unlock()
	return nil
}

func (l *loadExporter) Capabilities() Caps             { return Caps{Metrics: true, Events: true} }
func (l *loadExporter) Shutdown(context.Context) error { return nil }

func (l *loadExporter) dropTotal() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, v := range l.drops {
		n += v
	}
	return n
}

func (l *loadExporter) dropsByReason(reason string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drops[reason]
}

// pendingDrops reads the drop counters that have not been flushed into
// metric points yet. Conservation checks need both halves: a counter still
// in the emitter is damage that happened just as much as one that shipped.
func pendingDrops(s *Std) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, v := range s.dropCounts {
		n += v
	}
	return n
}

// countingEmitter is the tracker's sink when the measurement is about the
// tracker: it counts samples and keeps none, so Publish's cost is the
// tracker's own snapshot cost and not the Std emitter's buffering. The
// older BenchmarkTrackerPublish10k measures the pair together; keeping them
// separate is why the scaling series is readable.
type countingEmitter struct {
	records  atomic.Int64
	inflight atomic.Int64
}

var _ Emitter = (*countingEmitter)(nil)

func (c *countingEmitter) Record(context.Context, string, biz.Result, ...Option) {
	c.records.Add(1)
}

func (c *countingEmitter) SetInFlight(string, string, string, biz.Money, int64) {
	c.inflight.Add(1)
}

func benchRegistry(tb testing.TB) *registry.Registry {
	tb.Helper()
	r, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		tb.Fatal(err)
	}
	return &r
}

func newLoadEmitter(tb testing.TB, exp Exporter, opts ...EmitterOption) *Std {
	tb.Helper()
	opts = append([]EmitterOption{WithLogger(quietLogger())}, opts...)
	em, err := New(benchRegistry(tb), exp, opts...)
	if err != nil {
		tb.Fatal(err)
	}
	return em
}

// maxBenchContexts caps the context pool at roughly a quarter of a
// gigabyte. A run long enough to need more fails its conservation check
// rather than quietly reporting a suppression figure as an accept figure —
// the right direction for that failure.
const maxBenchContexts = 1 << 20

var (
	benchStages  = [...]string{"auth", "capture", "settle"}
	benchResults = [...]biz.Result{biz.ResultFailed, biz.ResultSuccess, biz.ResultDeferred, biz.ResultUnknown}
)

// benchContextsFor sizes the pool so a whole run of n Record calls mints
// keys the emitter has never seen. recordAt rotates entity fastest, then
// stage, then result, so n/(stages*results)+1 contexts cover the run.
//
// Sizing by the run length rather than by a fixed margin is what makes the
// exported-vs-called conservation check exact instead of probabilistic. The
// emitter's two-generation de-dup set retains up to 2*(1<<16) = 131072
// recent keys, but "recent" counts INSERTS, not calls — under a wedged
// backend most calls overflow before they ever reach the de-dup check, so
// the set turns over far more slowly than the call counter and any fixed
// pool eventually cycles back onto a key it still holds. That is not
// hypothetical: a 393216-key fixed pool leaked 0.24% of calls into the
// suppression path on the first slow-backend run, and the conservation
// check is what caught it.
func benchContextsFor(tb testing.TB, n int) []context.Context {
	tb.Helper()
	per := len(benchStages) * len(benchResults)
	size := n/per + 1
	if size < 64 {
		size = 64
	}
	if size > maxBenchContexts {
		size = maxBenchContexts
	}
	return benchContexts(tb, size)
}

func benchContexts(tb testing.TB, n int) []context.Context {
	tb.Helper()
	ctxs := make([]context.Context, n)
	for i := range ctxs {
		vc := biz.ValueContext{
			Flow:       "invoice.pay",
			EntityID:   fmt.Sprintf("inv_%07d", i),
			CustomerID: fmt.Sprintf("h:c%05d", i%4096),
			Segment:    []string{"smb", "enterprise"}[i%2],
			Money:      biz.Money{Amount: int64(100 + i%9000), Currency: "USD", Exponent: 2},
			Kind:       biz.KindFee,
		}
		ctx, err := biz.WithValueContext(context.Background(), vc)
		if err != nil {
			tb.Fatal(err)
		}
		ctxs[i] = ctx
	}
	return ctxs
}

// recordAt rotates entity, stage and result so successive calls mint
// distinct de-dup keys: entity turns over fastest, then stage, then result.
func recordAt(em *Std, ctxs []context.Context, n int64) {
	c := int64(len(ctxs))
	em.Record(
		ctxs[n%c],
		benchStages[(n/c)%int64(len(benchStages))],
		benchResults[(n/(c*int64(len(benchStages))))%int64(len(benchResults))],
	)
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

func trackerItemCount(tr *InFlightTracker) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.items)
}

const trackerSlots, trackerPerSlot = 256, 64

// BenchmarkTrackerTrackDoneParallel is the queue-consumer hot path under
// contention: Track on receive, Done on completion, from every consumer
// goroutine at once against one shared tracker. InFlightTracker is a map
// behind a single mutex, so this is where that mutex shows up. Sweep with
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

// BenchmarkTrackerPublishScale is how long the tracker holds its mutex on a
// publish, as the tracked set grows. Publish snapshots every item under
// t.mu, so this duration is also the worst-case stall a consumer goroutine
// eats on Track or Done when a publish lands — the number to compare
// against the publish cadence you configure.
//
// The sink is a counting emitter: this is the tracker's own snapshot cost,
// with none of the Std emitter's buffering folded in.
func BenchmarkTrackerPublishScale(b *testing.B) {
	for _, items := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("items=%d", items), func(b *testing.B) {
			ce := &countingEmitter{}
			tr := NewInFlightTracker(ce, WithTrackerLogger(quietLogger()))
			now := time.Now()
			for i := 0; i < items; i++ {
				tr.Track("invoice.pay", "capture", fmt.Sprintf("m%07d", i), usd(1499),
					now.Add(-time.Duration(i)*time.Second))
			}
			if n := trackerItemCount(tr); n != items {
				b.Fatalf("tracked %d items, want %d", n, items)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr.Publish()
			}
			b.StopTimer()

			// One (flow, stage, currency) combo, five ADR-0005 buckets,
			// one SetInFlight per bucket per publish. An exact count is
			// what keeps this from passing on zero iterations.
			if want := int64(b.N) * int64(len(AgeBuckets)); ce.inflight.Load() != want {
				b.Fatalf("emitted %d in-flight samples over %d publishes, want %d",
					ce.inflight.Load(), b.N, want)
			}
		})
	}
}

// TestSlowBackendReachesRecordAsCountedDropsNotAsBlocking pins the
// back-pressure contract: when the backend stops answering, the flusher
// blocks inside the exporter, the bounded buffer fills, and Record sheds
// load by dropping and counting (ADR-0002) — it never inherits the
// backend's latency.
//
// The proof is structural, not statistical. Nothing here sleeps and nothing
// compares durations: the exporter signals when it is inside the export,
// the buffer is then filled to a known depth, and the overflow count is
// exact arithmetic. If Record ever did block on the backend, this test
// deadlocks and the run times out — the one failure mode a timing threshold
// could not tell apart from a slow machine.
func TestSlowBackendReachesRecordAsCountedDropsNotAsBlocking(t *testing.T) {
	const bufSize = 64

	exp := newGatedLoadExporter()
	em := newLoadEmitter(t, exp, WithBufferSize(bufSize), WithFlushInterval(0))
	ctxs := benchContextsFor(t, 3*bufSize)

	var n int64
	fill := func(count int) {
		for i := 0; i < count; i++ {
			recordAt(em, ctxs, n)
			n++
		}
	}

	fill(bufSize) // buffer exactly full

	flushed := make(chan error, 1)
	go func() { flushed <- em.Flush(context.Background()) }()
	<-exp.entered // the flusher is inside the backend; the buffer is empty

	// The backend is wedged. These calls must all return: the first
	// bufSize refill the drained buffer, the next bufSize find it full and
	// are dropped as overflow.
	fill(2 * bufSize)

	close(exp.release)
	if err := <-flushed; err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := em.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got, want := exp.dropsByReason("overflow"), int64(bufSize); got != want {
		t.Fatalf("biz_dropped_events_total{reason=overflow} = %d, want %d", got, want)
	}
	if got := pendingDrops(em); got != 0 {
		t.Fatalf("%d drop counts never reached the exporter — a drop nobody can see is a silent one", got)
	}
	// Conservation: every call is either an exported outcome or a counted
	// drop. 3*bufSize issued, bufSize of them shed.
	if got, want := exp.events.Load(), int64(2*bufSize); got != want {
		t.Fatalf("exported %d outcomes, want %d (issued %d, dropped %d)",
			got, want, n, exp.dropsByReason("overflow"))
	}
}
