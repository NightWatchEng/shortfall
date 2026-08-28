package emit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

// Std is the standard Emitter implementation: non-blocking Record with a
// bounded event buffer and a bounded metric buffer, in-process
// de-duplication, ADR-0004 label enforcement, and loud drops
// (biz_dropped_events_total{reason}) — never silent ones.
//
// Ordering: batches may reach the exporter out of order when flushes
// overlap (background ticker plus caller-driven Flush). Consumers order
// by each point's/outcome's At — arrival order is explicitly NOT part of
// the contract.
type Std struct {
	reg    *registry.Registry
	exp    Exporter
	clock  func() time.Time
	logger *slog.Logger

	bufSize    int
	metricsCap int
	interval   time.Duration

	mu             sync.Mutex
	events         []biz.Outcome
	metrics        []MetricPoint
	dropCounts     map[string]int64 // reason -> delta since last flush
	metricOverflow int64            // dropped metric points since last flush (logged, not a money counter)
	dedup          *twoGenSet

	loopCtx    context.Context
	loopCancel context.CancelFunc
	stop       chan struct{}
	wg         sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

var _ Emitter = (*Std)(nil)

// EmitterOption configures New.
type EmitterOption func(*Std)

// WithClock injects the time source (tests need determinism).
func WithClock(now func() time.Time) EmitterOption { return func(s *Std) { s.clock = now } }

// WithBufferSize bounds the pending-event buffer (default 10000, ADR-0002).
// The metric buffer is bounded at 8x this value.
func WithBufferSize(n int) EmitterOption { return func(s *Std) { s.bufSize = n } }

// WithLogger sets the warning logger for label fallbacks and drops.
func WithLogger(l *slog.Logger) EmitterOption { return func(s *Std) { s.logger = l } }

// WithFlushInterval sets the background flush cadence; 0 disables the
// background flusher (callers drive Flush themselves — and own the
// obligation to call it, since buffers are bounded and overflow drops).
func WithFlushInterval(d time.Duration) EmitterOption { return func(s *Std) { s.interval = d } }

// Record option helpers — the concrete Options the frozen surface
// anticipated.

// WithSource sets the outcome's Source field.
func WithSource(source string) Option { return func(c *RecordConfig) { c.Source = source } }

// WithErr carries a short failure description onto the outcome.
func WithErr(err string) Option { return func(c *RecordConfig) { c.Err = err } }

// WithAt overrides the outcome's event time (provider event timestamps —
// hours-late webhook deliveries must not move money across windows).
func WithAt(at time.Time) Option { return func(c *RecordConfig) { c.At = at } }

// New builds a Std emitter over a validated registry and an exporter.
func New(reg *registry.Registry, exp Exporter, opts ...EmitterOption) (*Std, error) {
	if reg == nil || exp == nil {
		return nil, fmt.Errorf("emit: registry and exporter are required")
	}
	s := &Std{
		reg:        reg,
		exp:        exp,
		clock:      time.Now,
		logger:     slog.Default(),
		bufSize:    10000,
		interval:   time.Second,
		dropCounts: map[string]int64{},
		dedup:      newTwoGenSet(1 << 16),
		stop:       make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if s.bufSize < 1 {
		return nil, fmt.Errorf("emit: buffer size %d < 1", s.bufSize)
	}
	s.metricsCap = 8 * s.bufSize
	s.loopCtx, s.loopCancel = context.WithCancel(context.Background())
	if s.interval > 0 {
		s.wg.Add(1)
		go s.loop()
	}
	return s, nil
}

func (s *Std) loop() {
	defer s.wg.Done()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = s.Flush(s.loopCtx)
		case <-s.stop:
			return
		}
	}
}

// Record implements Emitter. It never blocks and never returns an error:
// anything unusable is dropped and counted (reason invalid); a full event
// buffer drops the WHOLE observation — event, metric increments, and
// de-dup memory — and counts it (reason overflow), so a retry after the
// buffer drains emits cleanly with no double-count; failed EVENT exports
// are counted at flush (reason export; metric-delta export failures are
// logged only — see Flush).
func (s *Std) Record(ctx context.Context, stage string, result biz.Result, opts ...Option) {
	vc, ok, decErr := biz.FromContext(ctx)
	if !ok {
		s.dropInvalid("record without usable ValueContext", decErr)
		return
	}
	var cfg RecordConfig
	for _, o := range opts {
		o(&cfg)
	}
	at := cfg.At
	if at.IsZero() {
		at = s.clock()
	}
	out := biz.Outcome{
		At:     at,
		VC:     vc,
		Stage:  stage,
		Result: result,
		Source: cfg.Source,
		Err:    cfg.Err,
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		out.TraceID = sc.TraceID().String()
	}
	if err := out.Validate(); err != nil {
		s.dropInvalid("outcome failed validation", err)
		return
	}

	// De-dup key DELIBERATELY includes the result — the proposal wrote
	// (flow, entity, stage), but the engine's realized leg de-duplicates
	// failures against LATER SUCCESS events for the same entity+stage,
	// so suppressing the transition here would corrupt realized loss.
	// Retries of the same outcome de-dup; transitions always emit.
	// Cross-process consequence (binds the engine legs): duplicate
	// SUCCESS events from different replicas both emit, so every
	// event-summing leg de-duplicates by entity, successes included.
	key := vc.Flow + "\x00" + vc.EntityID + "\x00" + stage + "\x00" + string(result)

	// Admission first, construction after: a suppressed or overflowing
	// call must not pay for label maps it will never use.
	s.mu.Lock()
	if len(s.events) >= s.bufSize {
		s.dropCounts["overflow"]++
		s.mu.Unlock()
		return
	}
	if s.dedup.seen(key) {
		s.mu.Unlock()
		return
	}
	s.events = append(s.events, out)
	s.mu.Unlock()

	flowLabel, stageLabel := s.flowStageLabels(vc.Flow, stage)
	segLabel := s.segmentLabel(vc.Segment)
	txnPoint := MetricPoint{
		Name: "biz_txn_total",
		Labels: map[string]string{
			"flow": flowLabel, "stage": stageLabel, "outcome": string(result),
			"currency": vc.Money.Currency, "segment": segLabel,
		},
		Value: 1,
		At:    at,
	}
	// biz_value_total is the REALIZED value sum: estimated amounts never
	// enter it (ADR-0004 froze its label set with no evidence axis, and
	// "realized and estimate never merged" is an invariant). The estimated
	// amount still rides the outcome EVENT, where the counterfactual leg
	// reads it. A count is fine either way.
	if vc.Estimated {
		s.appendMetrics(txnPoint)
	} else {
		valuePoint := MetricPoint{
			Name: "biz_value_total",
			Labels: map[string]string{
				"flow": flowLabel, "stage": stageLabel, "outcome": string(result),
				"currency": vc.Money.Currency, "kind": string(vc.Kind), "segment": segLabel,
			},
			Value: vc.Money.Amount,
			At:    at,
		}
		s.appendMetrics(valuePoint, txnPoint)
	}
}

// SetInFlight implements Emitter: one gauge sample per (flow, stage,
// bucket, currency). Every label crosses the same fences Record uses:
// unknown buckets are rejected loudly, and flow/stage outside the
// registry fall back to "unregistered" — no caller string ever mints an
// unbounded series (ADR-0004).
func (s *Std) SetInFlight(flow, stage, ageBucket string, money biz.Money, count int64) {
	valid := false
	for _, b := range AgeBuckets {
		if ageBucket == b {
			valid = true
			break
		}
	}
	if !valid || money.Validate() != nil || count < 0 {
		s.dropInvalid("in-flight gauge rejected", fmt.Errorf("bucket %q / money %+v / count %d", ageBucket, money, count))
		return
	}
	flowLabel, stageLabel := s.flowStageLabels(flow, stage)
	lbls := func() map[string]string {
		return map[string]string{
			"flow": flowLabel, "stage": stageLabel, "age_bucket": ageBucket, "currency": money.Currency,
		}
	}
	now := s.clock()
	// Value and count are the two gauges of the in-flight family (ADR-0012),
	// published together from the same snapshot so they never disagree. Each
	// point gets its own label map — no shared-map aliasing downstream.
	s.appendMetrics(
		MetricPoint{Name: "biz_inflight_value", Labels: lbls(), Value: money.Amount, At: now},
		MetricPoint{Name: "biz_inflight_count", Labels: lbls(), Value: count, At: now},
	)
}

// Flush exports everything pending and returns the first export error.
// Failed EVENT batches are dropped and counted (reason export). Failed
// METRIC batches are logged and dropped WITHOUT counting — re-queuing
// deltas risks double-count on partial writes — except the
// biz_dropped_events_total points themselves, which are re-credited so a
// backend outage can never destroy the record of its own damage.
func (s *Std) Flush(ctx context.Context) error {
	s.mu.Lock()
	events := s.events
	s.events = nil
	metrics := s.metrics
	s.metrics = nil
	if s.metricOverflow > 0 {
		s.logger.Warn("emit: metric buffer overflowed; points dropped", "points", s.metricOverflow)
		s.metricOverflow = 0
	}
	for reason, n := range s.dropCounts {
		metrics = append(metrics, MetricPoint{
			Name:   "biz_dropped_events_total",
			Labels: map[string]string{"reason": reason},
			Value:  n,
			At:     s.clock(),
		})
	}
	s.dropCounts = map[string]int64{}
	s.mu.Unlock()

	var firstErr error
	if len(events) > 0 {
		if err := s.exp.ExportEvents(ctx, events); err != nil {
			firstErr = err
			s.logger.Warn("emit: event export failed; batch dropped and counted", "error", err, "dropped", len(events))
			s.mu.Lock()
			s.dropCounts["export"] += int64(len(events))
			s.mu.Unlock()
		}
	}
	if len(metrics) > 0 {
		if err := s.exp.ExportMetrics(ctx, metrics); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.logger.Warn("emit: metric export failed; batch dropped", "error", err, "points", len(metrics))
			// Preserve the drop counters: they are the record of damage
			// and never left the process, so re-crediting cannot
			// double-count.
			s.mu.Lock()
			for _, p := range metrics {
				if p.Name == "biz_dropped_events_total" {
					s.dropCounts[p.Labels["reason"]] += p.Value
				}
			}
			s.mu.Unlock()
		}
	}
	return firstErr
}

// Close is idempotent: the first call stops the background flusher
// (cancelling any in-flight background export so Close can honor its
// ctx), performs a final flush with the CALLER's ctx, and shuts the
// exporter down; later calls return the first result. Terminal limit,
// stated honestly: if the backend is down at Close, the final counters
// remain un-exported — they survive in memory until process exit and in
// the warning log, and reconciliation is the backstop.
func (s *Std) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.loopCancel()
		s.wg.Wait()
		flushErr := s.Flush(ctx)
		// One more pass so export-failure counters from the final flush
		// get their chance to ship when only the event half failed.
		flush2Err := s.Flush(ctx)
		s.closeErr = errors.Join(flushErr, flush2Err, s.exp.Shutdown(ctx))
	})
	return s.closeErr
}

func (s *Std) appendMetrics(points ...MetricPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range points {
		if len(s.metrics) >= s.metricsCap {
			s.metricOverflow++
			continue
		}
		s.metrics = append(s.metrics, p)
	}
}

func (s *Std) dropInvalid(msg string, err error) {
	s.logger.Warn("emit: "+msg+" — dropped and counted", "error", err)
	s.mu.Lock()
	s.dropCounts["invalid"]++
	s.mu.Unlock()
}

// flowStageLabels applies the ADR-0004 fence shared by Record and
// SetInFlight: names outside the registry emit as the fixed value
// "unregistered" — sums stay complete, cardinality stays bounded, the
// misconfiguration is visible on a dashboard.
func (s *Std) flowStageLabels(flow, stage string) (flowLabel, stageLabel string) {
	flowLabel, stageLabel = "unregistered", "unregistered"
	if f, ok := s.reg.Flow(flow); ok {
		flowLabel = flow
		if f.StageValid(stage) {
			stageLabel = stage
		} else {
			s.logger.Warn("emit: stage not in registry; metrics use fallback", "flow", flow, "stage", stage)
		}
	} else {
		s.logger.Warn("emit: flow not in registry; metrics use fallback", "flow", flow)
	}
	return flowLabel, stageLabel
}

// segmentLabel: outside the enumeration emits as "" with a warning.
func (s *Std) segmentLabel(segment string) string {
	if segment == "" {
		return ""
	}
	if s.reg.SegmentValid(segment) {
		return segment
	}
	s.logger.Warn("emit: segment outside enumeration; label dropped", "segment", segment)
	return ""
}

// twoGenSet is a bounded approximate-LRU: two generations of maps, the
// older discarded when the newer fills (its buckets are cleared and
// reused, so rotation allocates nothing in steady state). Amortized
// allocation-light; exactly as strong as in-process retry de-dup needs
// to be — cross-process de-dup is the engine's event-side job.
type twoGenSet struct {
	cap       int
	cur, prev map[string]struct{}
}

func newTwoGenSet(capacity int) *twoGenSet {
	return &twoGenSet{cap: capacity, cur: make(map[string]struct{}, capacity)}
}

// seen reports whether key was recorded recently, recording it if not.
// Caller holds the emitter lock.
func (t *twoGenSet) seen(key string) bool {
	if _, ok := t.cur[key]; ok {
		return true
	}
	if _, ok := t.prev[key]; ok {
		return true
	}
	if len(t.cur) >= t.cap {
		old := t.prev
		t.prev = t.cur
		if old != nil {
			clear(old)
			t.cur = old
		} else {
			t.cur = make(map[string]struct{}, t.cap)
		}
	}
	t.cur[key] = struct{}{}
	return false
}
