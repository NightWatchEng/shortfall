package testkit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/registry"
)

// Sustained-load harness. Benchmarks answer "how fast is one call";
// this answers "what happens after the run keeps going": a leak, an
// unbounded buffer, a metric label set growing with transaction count,
// an exporter queue backing up. Ledger transactions from the seeded
// checkout system are replayed through the REAL instrumentation path —
// emit.Std with a background flush loop plus an InFlightTracker — and
// at the end the sink must reconcile against what was issued: no
// outcome event lost, none doubled, no drop counter ticked, nothing
// left in flight.
//
// Two configurations, both documented in docs/performance.md:
//   - the default smoke (seconds, runs in the ordinary `go test` gate);
//   - a long soak, selected by environment variables:
//     SHORTFALL_LOAD_RATE (tx/s), SHORTFALL_LOAD_SECONDS,
//     SHORTFALL_LOAD_WORKERS.
// Read a smoke result as a smoke result: it catches reconciliation
// breaks and gross leaks, not slow ones. The soak is a release check,
// not a PR gate.

// loadSink is the exporter the load run drains into: it counts and
// sums what it is handed, tracks distinct metric series keys (the
// cardinality assertion reads it), and keeps nothing else, so heap
// growth in a run points at the library, not the sink.
type loadSink struct {
	events      atomic.Int64
	amountMinor atomic.Int64

	mu       sync.Mutex
	series   map[string]struct{}
	dropped  int64            // biz_dropped_events_total observed on the wire
	inflight map[string]int64 // latest value per inflight series
}

func newLoadSink() *loadSink {
	return &loadSink{series: map[string]struct{}{}, inflight: map[string]int64{}}
}

func (s *loadSink) ExportMetrics(_ context.Context, batch []emit.MetricPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range batch {
		key := p.Name
		for _, lk := range sortedKeys(p.Labels) {
			key += "|" + lk + "=" + p.Labels[lk]
		}
		s.series[key] = struct{}{}
		switch p.Name {
		case "biz_dropped_events_total":
			s.dropped += p.Value
		case "biz_inflight_value", "biz_inflight_count":
			s.inflight[key] = p.Value
		}
	}
	return nil
}

func (s *loadSink) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	var minor int64
	for _, o := range batch {
		minor += o.VC.Money.Amount
	}
	s.events.Add(int64(len(batch)))
	s.amountMinor.Add(minor)
	return nil
}

func (s *loadSink) Capabilities() emit.Caps { return emit.Caps{Metrics: true, Events: true} }

func (s *loadSink) Shutdown(context.Context) error { return nil }

func (s *loadSink) seriesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.series)
}

func (s *loadSink) droppedTotal() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *loadSink) inflightResidue() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var residue int64
	for _, v := range s.inflight {
		residue += v
	}
	return residue
}

type loadConfig struct {
	rate     float64 // offered transactions per second, all workers together
	duration time.Duration
	workers  int
	// leakEvery seeds a deliberate leak every N issued transactions —
	// a goroutine pinned alive holding a buffer. It exists so the
	// flatness assertions can be shown to fail on a real leak; zero
	// disables it.
	leakEvery int
}

type loadSample struct {
	at         time.Duration
	heapInuse  uint64
	goroutines int
	series     int
	gcPauseNs  uint64
}

type loadReport struct {
	issued       int64
	issuedMinor  int64
	achievedRate float64
	latencies    []time.Duration // sampled Record-call-site latencies, sorted
	samples      []loadSample
	problems     []string
}

func (r *loadReport) problemf(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *loadReport) latencyAt(q float64) time.Duration {
	if len(r.latencies) == 0 {
		return 0
	}
	i := int(q * float64(len(r.latencies)-1))
	return r.latencies[i]
}

// runSustainedLoad replays seeded checkout-ledger transactions through a
// real emitter and tracker at the offered rate, then reconciles and
// checks the run's second half for growth. It reports problems instead
// of failing so its own failure detection is testable.
func runSustainedLoad(tb testing.TB, cfg loadConfig) loadReport {
	tb.Helper()
	var report loadReport

	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		tb.Fatal(err)
	}
	sink := newLoadSink()
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	em, err := emit.New(&reg, sink,
		emit.WithLogger(quiet),
		emit.WithFlushInterval(50*time.Millisecond),
	)
	if err != nil {
		tb.Fatal(err)
	}
	tracker := emit.NewInFlightTracker(em, emit.WithTrackerLogger(quiet))
	tracker.Start(25 * time.Millisecond)

	// Ground truth: one seeded, fault-free week. Only terminal states
	// replay — the driver models completed stage transitions.
	res := checkout.Run(checkout.Config{
		Seed:  7,
		Start: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
	})
	var pool []checkout.Txn
	for _, txn := range res.Ledger.Txns {
		switch txn.State {
		case checkout.StateSettled, checkout.StateAuthFail, checkout.StateCapFail:
			pool = append(pool, txn)
		}
	}
	if len(pool) == 0 {
		tb.Fatal("seeded ledger produced no terminal transactions")
	}

	var (
		issued      atomic.Int64
		issuedMinor atomic.Int64
		next        atomic.Int64
		latMu       sync.Mutex
		lats        []time.Duration
		leaked      []chan struct{}
		leakMu      sync.Mutex
	)
	perWorker := cfg.rate / float64(cfg.workers)
	interval := time.Duration(float64(time.Second) / perWorker)

	stopSampling := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		tick := time.NewTicker(cfg.duration / 24)
		defer tick.Stop()
		started := time.Now()
		var ms runtime.MemStats
		for {
			select {
			case <-stopSampling:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				report.samples = append(report.samples, loadSample{
					at:         time.Since(started),
					heapInuse:  ms.HeapInuse,
					goroutines: runtime.NumGoroutine(),
					series:     sink.seriesCount(),
					gcPauseNs:  ms.PauseTotalNs,
				})
			}
		}
	}()

	start := time.Now()
	deadline := start.Add(cfg.duration)
	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for now := range tick.C {
				if now.After(deadline) {
					return
				}
				n := next.Add(1)
				txn := pool[int(n)%len(pool)]
				// Unique per-replay entity id: reconciliation needs
				// every issued outcome to be its own entity, or the
				// emitter's retry suppression would (correctly) fold
				// replays of one ledger row together.
				id := fmt.Sprintf("%s-r%d", txn.ID, n)
				money := biz.Money{Amount: txn.AmountMinor, Currency: txn.Currency, Exponent: 2}
				tracker.Track("invoice.pay", "capture", id, money, now)
				ctx, err := biz.WithValueContext(context.Background(), biz.ValueContext{
					Flow:       "invoice.pay",
					EntityID:   id,
					CustomerID: txn.CustomerID,
					Segment:    string(txn.Segment),
					Money:      money,
					Kind:       biz.KindFee,
				})
				if err != nil {
					tb.Error(err)
					return
				}
				result := biz.ResultSuccess
				if txn.State != checkout.StateSettled {
					result = biz.ResultFailed
				}
				recordStart := time.Now()
				em.Record(ctx, "capture", result)
				lat := time.Since(recordStart)
				tracker.Done("invoice.pay", "capture", id)
				issued.Add(1)
				issuedMinor.Add(txn.AmountMinor)
				if n%8 == 0 {
					latMu.Lock()
					lats = append(lats, lat)
					latMu.Unlock()
				}
				if cfg.leakEvery > 0 && n%int64(cfg.leakEvery) == 0 {
					hold := make(chan struct{})
					leakMu.Lock()
					leaked = append(leaked, hold)
					leakMu.Unlock()
					go func() { // the seeded defect: pinned goroutine + buffer
						buf := make([]byte, 1<<18)
						<-hold
						_ = buf
					}()
				}
			}
		}(w)
	}
	wg.Wait()
	close(stopSampling)
	samplerWG.Wait()
	elapsed := time.Since(start)

	// Quiesce: a final publish zeroes the emptied combos, a final flush
	// drains the buffer, then the emitter closes.
	tracker.Close()
	tracker.Publish()
	if err := em.Flush(context.Background()); err != nil {
		report.problemf("final flush: %v", err)
	}
	if err := em.Close(context.Background()); err != nil {
		report.problemf("close: %v", err)
	}

	report.issued = issued.Load()
	report.issuedMinor = issuedMinor.Load()
	report.achievedRate = float64(report.issued) / elapsed.Seconds()
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	report.latencies = lats

	// Reconciliation against ground truth: this is the assertion that
	// matters more than any throughput number. A fast library that
	// drops money events under load is worse than a slow one.
	if got := sink.events.Load(); got != report.issued {
		report.problemf("outcome events: sink received %d, ledger replay issued %d", got, report.issued)
	}
	if got := sink.amountMinor.Load(); got != report.issuedMinor {
		report.problemf("outcome value: sink summed %d minor units, replay issued %d", got, report.issuedMinor)
	}
	if d := sink.droppedTotal(); d != 0 {
		report.problemf("biz_dropped_events_total = %d on the wire; the accept path was not the whole path", d)
	}
	if tracker.Overflowed() != 0 || tracker.Rejected() != 0 {
		report.problemf("tracker overflowed=%d rejected=%d", tracker.Overflowed(), tracker.Rejected())
	}
	if residue := sink.inflightResidue(); residue != 0 {
		report.problemf("in-flight gauges left nonzero after quiesce: residue %d", residue)
	}

	// Unbounded-growth assertions over the run's second half. Heap is
	// GC-sawtoothed, so halves compare by median; goroutines and series
	// must be flat outright.
	if n := len(report.samples); n >= 8 {
		half := report.samples[n/2:]
		firstHalf := report.samples[:n/2]
		medHeap := func(ss []loadSample) uint64 {
			hs := make([]uint64, len(ss))
			for i, s := range ss {
				hs[i] = s.heapInuse
			}
			sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
			return hs[len(hs)/2]
		}
		const heapSlack = 4 << 20
		if a, b := medHeap(firstHalf), medHeap(half); b > a+a/3+heapSlack {
			report.problemf("heap growth: median HeapInuse %d in the first half, %d in the second", a, b)
		}
		grMin, grMax := half[0].goroutines, half[0].goroutines
		for _, s := range half {
			grMin = min(grMin, s.goroutines)
			grMax = max(grMax, s.goroutines)
		}
		if grMax > grMin+cfg.workers {
			report.problemf("goroutine growth in the second half: %d -> %d", grMin, grMax)
		}
		if a, b := half[0].series, half[len(half)-1].series; b > a {
			report.problemf("metric series still growing in the second half: %d -> %d (cardinality must not follow transaction count)", a, b)
		}
	}

	leakMu.Lock()
	for _, hold := range leaked {
		close(hold)
	}
	leakMu.Unlock()
	return report
}

func loadEnv(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// TestSustainedLoadReconciles is the smoke configuration: seconds long,
// CI-affordable, and exact — every replayed ledger transaction must come
// out the far side of the real emitter, once. The env variables select
// the long soak; docs/performance.md states both configurations.
func TestSustainedLoadReconciles(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained load skipped in -short")
	}
	cfg := loadConfig{
		rate:     loadEnv("SHORTFALL_LOAD_RATE", 1200),
		duration: time.Duration(loadEnv("SHORTFALL_LOAD_SECONDS", 3) * float64(time.Second)),
		workers:  int(loadEnv("SHORTFALL_LOAD_WORKERS", 8)),
	}
	report := runSustainedLoad(t, cfg)
	for _, p := range report.problems {
		t.Error(p)
	}
	t.Logf("sustained load: issued=%d offered=%.0f/s achieved=%.0f/s p50=%v p99=%v max=%v samples=%d",
		report.issued, cfg.rate, report.achievedRate,
		report.latencyAt(0.50), report.latencyAt(0.99), report.latencyAt(1.0), len(report.samples))
}

// TestSustainedLoadCatchesSeededLeak proves the flatness assertions can
// fail: a run with a deliberately pinned goroutine-and-buffer leak must
// come back with problems. Without this, the growth checks could rot
// into assertions that pass on anything.
func TestSustainedLoadCatchesSeededLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained load skipped in -short")
	}
	report := runSustainedLoad(t, loadConfig{
		rate:      800,
		duration:  2 * time.Second,
		workers:   4,
		leakEvery: 10,
	})
	if len(report.problems) == 0 {
		t.Fatal("a seeded goroutine+buffer leak produced no problems; the growth assertions are vacuous")
	}
	t.Logf("seeded leak detected as: %v", report.problems)
}
