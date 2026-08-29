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
// emit.InFlightTracker.
//
// This file holds the shared harness, the one gate-resident addition
// (BenchmarkTrackerPublishScale), and the deterministic back-pressure test.
// Everything that uses b.RunParallel lives in concurrency_load_bench_test.go
// behind the `benchload` build tag.
//
// That split is measured, not assumed. scripts/ci-bench.sh runs the gate at
// BENCH_TIME=1x, so b.N is 1 — and RunParallel then spawns GOMAXPROCS
// goroutines to perform a single iteration, which measures scheduler
// wake-up rather than Record. Two runs of this package at the CI settings
// over an IDENTICAL tree put the parallel benchmarks at ±350% and ±454%
// B/op with allocs/op swinging 68-85 against the 17 the accept path
// actually costs, and benchstat called one of them a 32% regression at
// p=0.041 on a diff that did not exist. TrackerPublishScale over the same
// pair was bit-identical on every sample. A benchmark that manufactures
// regressions is worse than no benchmark, so the noisy ones are tagged out
// and read from an explicit -cpu sweep instead; docs/performance.md carries
// the commands and says which set is which.

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
	// No delay here, deliberately. Flush exports events and then metrics on
	// one goroutine, and the metric batch is non-empty on essentially every
	// flush (the drop counters always append a point), so sleeping in both
	// would make a "25 ms backend" cost 50 ms per flush cycle — twice what
	// the delay field, the benchmark's name, and the published table all
	// say. Caught in review; the doubled stall was silently inflating the
	// published drop percentage.
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

func trackerItemCount(tr *InFlightTracker) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.items)
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
