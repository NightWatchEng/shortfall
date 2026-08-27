package emit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

// Std is the standard Emitter implementation: non-blocking Record with
// bounded buffers, in-process de-duplication, ADR-0004 label enforcement,
// and loud drops (biz_dropped_events_total{reason}) — never silent ones.
type Std struct {
	reg    *registry.Registry
	exp    Exporter
	clock  func() time.Time
	logger *slog.Logger

	bufSize  int
	interval time.Duration

	mu         sync.Mutex
	events     []biz.Outcome
	metrics    []MetricPoint
	dropCounts map[string]int64 // reason -> delta since last flush
	dedup      *twoGenSet

	stop    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup
}

var _ Emitter = (*Std)(nil)

// EmitterOption configures New.
type EmitterOption func(*Std)

// WithClock injects the time source (tests need determinism).
func WithClock(now func() time.Time) EmitterOption { return func(s *Std) { s.clock = now } }

// WithBufferSize bounds the pending-event buffer (default 10000, ADR-0002).
func WithBufferSize(n int) EmitterOption { return func(s *Std) { s.bufSize = n } }

// WithLogger sets the warning logger for label fallbacks and drops.
func WithLogger(l *slog.Logger) EmitterOption { return func(s *Std) { s.logger = l } }

// WithFlushInterval sets the background flush cadence; 0 disables the
// background flusher (callers drive Flush themselves).
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
			s.Flush(context.Background())
		case <-s.stop:
			return
		}
	}
}

// Record implements Emitter. It never blocks and never returns an error:
// anything unusable is dropped and counted (reason invalid), a full
// buffer drops the event and counts it (reason overflow) while the
// bounded metric increments still flow, and export failures are counted
// at flush (reason export).
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
	if err := out.Validate(); err != nil {
		s.dropInvalid("outcome failed validation", err)
		return
	}

	flowLabel, stageLabel, segLabel := s.labels(vc, stage)
	points := [2]MetricPoint{
		{
			Name: "biz_value_total",
			Labels: map[string]string{
				"flow": flowLabel, "stage": stageLabel, "outcome": string(result),
				"currency": vc.Money.Currency, "kind": string(vc.Kind), "segment": segLabel,
			},
			Value: vc.Money.Amount,
			At:    at,
		},
		{
			Name: "biz_txn_total",
			Labels: map[string]string{
				"flow": flowLabel, "stage": stageLabel, "outcome": string(result),
				"currency": vc.Money.Currency, "segment": segLabel,
			},
			Value: 1,
			At:    at,
		},
	}

	key := vc.Flow + "\x00" + vc.EntityID + "\x00" + stage + "\x00" + string(result)

	s.mu.Lock()
	defer s.mu.Unlock()
	// De-dup key DELIBERATELY includes the result — the proposal wrote
	// (flow, entity, stage), but the engine's realized leg de-duplicates
	// failures against LATER SUCCESS events for the same entity+stage,
	// so suppressing the transition here would corrupt realized loss.
	// Retries of the same outcome de-dup; transitions always emit.
	if s.dedup.seen(key) {
		return
	}
	s.metrics = append(s.metrics, points[0], points[1])
	if len(s.events) >= s.bufSize {
		s.dropCounts["overflow"]++
		return
	}
	s.events = append(s.events, out)
}

// SetInFlight implements Emitter: one gauge sample per (flow, stage,
// bucket, currency). Unknown buckets are rejected loudly — a typo must
// never mint a sixth series past the ADR-0004 fence.
func (s *Std) SetInFlight(flow, stage, ageBucket string, money biz.Money) {
	valid := false
	for _, b := range AgeBuckets {
		if ageBucket == b {
			valid = true
			break
		}
	}
	if !valid || money.Validate() != nil {
		s.dropInvalid("in-flight gauge rejected", fmt.Errorf("bucket %q / money %+v", ageBucket, money))
		return
	}
	p := MetricPoint{
		Name: "biz_inflight_value",
		Labels: map[string]string{
			"flow": flow, "stage": stage, "age_bucket": ageBucket, "currency": money.Currency,
		},
		Value: money.Amount,
		At:    s.clock(),
	}
	s.mu.Lock()
	s.metrics = append(s.metrics, p)
	s.mu.Unlock()
}

// Flush exports everything pending. Export failures drop the batch and
// count it (reason export) — the counter itself rides the next metric
// flush, so a backend outage becomes a visible number, not a silence.
func (s *Std) Flush(ctx context.Context) {
	s.mu.Lock()
	events := s.events
	s.events = nil
	metrics := s.metrics
	s.metrics = nil
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

	if len(events) > 0 {
		if err := s.exp.ExportEvents(ctx, events); err != nil {
			s.logger.Warn("emit: event export failed; batch dropped and counted", "error", err, "dropped", len(events))
			s.mu.Lock()
			s.dropCounts["export"] += int64(len(events))
			s.mu.Unlock()
		}
	}
	if len(metrics) > 0 {
		if err := s.exp.ExportMetrics(ctx, metrics); err != nil {
			// Metric deltas cannot be re-queued without double-count risk
			// on partial writes; log loudly. The events counter above is
			// the money-accounting fence; reconciliation catches residue.
			s.logger.Warn("emit: metric export failed; batch dropped", "error", err, "points", len(metrics))
		}
	}
}

// Close flushes and shuts the exporter down.
func (s *Std) Close(ctx context.Context) error {
	s.stopped.Do(func() { close(s.stop) })
	s.wg.Wait()
	s.Flush(ctx)
	// A failed final event flush re-queues only counters; surface them.
	s.Flush(ctx)
	return s.exp.Shutdown(ctx)
}

func (s *Std) dropInvalid(msg string, err error) {
	s.logger.Warn("emit: "+msg+" — dropped and counted", "error", err)
	s.mu.Lock()
	s.dropCounts["invalid"]++
	s.mu.Unlock()
}

// labels applies ADR-0004 enforcement: flow/stage outside the registry
// emit as the fixed value "unregistered" (sums stay complete, cardinality
// stays bounded, the misconfiguration is visible); a segment outside the
// enumeration emits as "" — and the outcome EVENT always keeps the raw
// names for diagnosis.
func (s *Std) labels(vc biz.ValueContext, stage string) (flowLabel, stageLabel, segLabel string) {
	flowLabel, stageLabel = "unregistered", "unregistered"
	if f, ok := s.reg.Flow(vc.Flow); ok {
		flowLabel = vc.Flow
		if f.StageValid(stage) {
			stageLabel = stage
		} else {
			s.logger.Warn("emit: stage not in registry; metrics use fallback", "flow", vc.Flow, "stage", stage)
		}
	} else {
		s.logger.Warn("emit: flow not in registry; metrics use fallback", "flow", vc.Flow)
	}
	if vc.Segment != "" && s.reg.SegmentValid(vc.Segment) {
		segLabel = vc.Segment
	} else if vc.Segment != "" {
		s.logger.Warn("emit: segment outside enumeration; label dropped", "segment", vc.Segment)
	}
	return flowLabel, stageLabel, segLabel
}

// twoGenSet is a bounded approximate-LRU: two generations of maps, the
// older discarded when the newer fills. Cheap, allocation-light, and
// exactly as strong as in-process retry de-dup needs to be (cross-process
// de-dup is the engine's event-side job).
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
		t.prev = t.cur
		t.cur = make(map[string]struct{}, t.cap)
	}
	t.cur[key] = struct{}{}
	return false
}
