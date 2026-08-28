package emit

import (
	"log/slog"
	"sync"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// AgeBucketFor maps a message age onto the fixed ADR-0005 buckets.
// Intervals are left-closed, right-open — [1m, 5m): exactly 1m is 1m-5m,
// exactly 2h is gt2h. Negative ages (producer/consumer clock skew) clamp
// to the youngest bucket — skew must never invent old backlog.
func AgeBucketFor(age time.Duration) string {
	switch {
	case age < time.Minute:
		return AgeLt1m
	case age < 5*time.Minute:
		return Age1mTo5m
	case age < 30*time.Minute:
		return Age5mTo30m
	case age < 2*time.Hour:
		return Age30mTo2h
	default:
		return AgeGt2h
	}
}

// InFlightTracker maintains the current in-flight set for queue stages
// and publishes biz_inflight_value levels — the single best
// queue-degradation signal you can put on a pager. Wire it into a
// consumer wrapper: Track on receive/enqueue, Done on completion, and
// either Start a publish loop or call Publish on your own cadence.
//
// Age semantics: measured from the FIRST enqueue timestamp — a retry
// re-Track of the same id never makes the backlog look younger.
type InFlightTracker struct {
	em     Emitter
	clock  func() time.Time
	logger *slog.Logger

	mu    sync.Mutex
	items map[inflightKey]inflightItem
	// combos we have published non-zero values for and must zero once
	// when they empty, so a stalled dashboard never shows stale levels.
	live map[comboKey]struct{}
	// canonical exponent per currency, pinned on first sight: a second
	// exponent for the same currency would silently flap one gauge
	// series between incomparable sums (a confirmed finding).
	exponents map[string]int8
	maxItems  int
	overflow  int64 // Track CALLS the bound rejected (retries count each)
	rejected  int64 // Track calls rejected for invalid/mismatched money

	// publishMu serializes snapshot AND emission: without it an older
	// snapshot can be emitted after a newer one and win under the
	// order-by-At contract (a confirmed finding).
	publishMu sync.Mutex

	started  bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type inflightKey struct{ flow, stage, id string }

type comboKey struct {
	flow, stage, currency string
	exponent              int8
}

type inflightItem struct {
	money      biz.Money
	enqueuedAt time.Time
}

// TrackerOption configures NewInFlightTracker.
type TrackerOption func(*InFlightTracker)

// WithTrackerClock injects the time source (tests need determinism).
func WithTrackerClock(now func() time.Time) TrackerOption {
	return func(t *InFlightTracker) { t.clock = now }
}

// WithTrackerMaxItems bounds the tracked set (default 1<<20). Beyond the
// bound, Track calls are dropped, logged, and counted — the published
// value is then an UNDERSTATEMENT and Overflowed() reports the rejected
// Track CALLS (a retried message counts once per attempt). Values below
// 1 are replaced by the default, loudly. Memory note: Go maps retain
// their high-water bucket memory, so this bound is also the worst-case
// resident footprint after an incident-sized backlog drains.
func WithTrackerMaxItems(n int) TrackerOption {
	return func(t *InFlightTracker) { t.maxItems = n }
}

// WithTrackerLogger sets the warning logger for rejected and overflowed
// tracks (default slog.Default) — drops are loud here like everywhere
// else in this package.
func WithTrackerLogger(l *slog.Logger) TrackerOption {
	return func(t *InFlightTracker) { t.logger = l }
}

// NewInFlightTracker builds a tracker publishing through em.
func NewInFlightTracker(em Emitter, opts ...TrackerOption) *InFlightTracker {
	t := &InFlightTracker{
		em:        em,
		clock:     time.Now,
		logger:    slog.Default(),
		items:     map[inflightKey]inflightItem{},
		live:      map[comboKey]struct{}{},
		exponents: map[string]int8{},
		maxItems:  1 << 20,
		stop:      make(chan struct{}),
	}
	for _, o := range opts {
		o(t)
	}
	if t.maxItems < 1 {
		t.logger.Warn("emit: tracker max items below 1; using the default", "requested", t.maxItems)
		t.maxItems = 1 << 20
	}
	return t
}

// Track records a message entering a stage. Re-tracking an id keeps the
// OLDEST enqueue time seen (retries never rejuvenate backlog; an earlier
// timestamp on a retrack is adopted as better information) and updates
// the money (amounts should not change; last write wins if they do).
// Rejections are loud: invalid money and a currency re-tracked under a
// different exponent are logged and counted (Rejected()).
func (t *InFlightTracker) Track(flow, stage, id string, money biz.Money, enqueuedAt time.Time) {
	if err := money.Validate(); err != nil {
		t.logger.Warn("emit: tracker rejected invalid money — dropped and counted", "error", err)
		t.mu.Lock()
		t.rejected++
		t.mu.Unlock()
		return
	}
	k := inflightKey{flow, stage, id}
	t.mu.Lock()
	defer t.mu.Unlock()
	if pinned, ok := t.exponents[money.Currency]; ok {
		if pinned != money.Exponent {
			t.rejected++
			t.logger.Warn("emit: tracker rejected mismatched currency exponent — one series must never flap between incomparable sums",
				"currency", money.Currency, "pinned", pinned, "got", money.Exponent)
			return
		}
	} else {
		t.exponents[money.Currency] = money.Exponent
	}
	if prev, ok := t.items[k]; ok {
		if prev.enqueuedAt.Before(enqueuedAt) {
			enqueuedAt = prev.enqueuedAt
		}
		t.items[k] = inflightItem{money: money, enqueuedAt: enqueuedAt}
		return
	}
	if len(t.items) >= t.maxItems {
		t.overflow++
		return
	}
	t.items[k] = inflightItem{money: money, enqueuedAt: enqueuedAt}
}

// Done records a message leaving its stage. Unknown ids are a no-op —
// Done is idempotent by design (consumer wrappers retry).
func (t *InFlightTracker) Done(flow, stage, id string) {
	t.mu.Lock()
	delete(t.items, inflightKey{flow, stage, id})
	t.mu.Unlock()
}

// Overflowed returns how many Track CALLS the bound rejected since the
// tracker was built: nonzero means the published gauge UNDERSTATES the
// true in-flight value.
func (t *InFlightTracker) Overflowed() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overflow
}

// Rejected returns how many Track calls were refused for invalid money
// or a mismatched currency exponent.
func (t *InFlightTracker) Rejected() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rejected
}

// Publish computes the current per-(flow, stage, bucket, currency) sums
// and pushes every bucket of every live combo — including zeroes, so a
// bucket that empties reads 0 instead of holding its last level. A combo
// with no items is zeroed one final time and then retired from
// publishing (no forever-zero churn).
func (t *InFlightTracker) Publish() {
	// One publisher at a time, snapshot through emission: overlapping
	// publishers could otherwise emit an older snapshot with newer At
	// stamps, and the stale level would win under order-by-At.
	t.publishMu.Lock()
	defer t.publishMu.Unlock()

	now := t.clock()

	type bucketAgg struct {
		minor int64 // summed minor units
		count int64 // number of in-flight transactions
	}
	type comboBuckets map[string]*bucketAgg // bucket -> value+count
	t.mu.Lock()
	sums := map[comboKey]comboBuckets{}
	for k, item := range t.items {
		ck := comboKey{k.flow, k.stage, item.money.Currency, item.money.Exponent}
		cb, ok := sums[ck]
		if !ok {
			cb = comboBuckets{}
			sums[ck] = cb
		}
		bucket := AgeBucketFor(now.Sub(item.enqueuedAt))
		a := cb[bucket]
		if a == nil {
			a = &bucketAgg{}
			cb[bucket] = a
		}
		a.minor += item.money.Amount
		a.count++
	}
	// Every combo with items stays live; every live combo without items
	// gets one zeroing pass and retires.
	toPublish := make([]comboKey, 0, len(sums))
	for ck := range sums {
		t.live[ck] = struct{}{}
		toPublish = append(toPublish, ck)
	}
	var toRetire []comboKey
	for ck := range t.live {
		if _, ok := sums[ck]; !ok {
			toRetire = append(toRetire, ck)
			toPublish = append(toPublish, ck)
		}
	}
	for _, ck := range toRetire {
		delete(t.live, ck)
	}
	overflowed := t.overflow
	t.mu.Unlock()
	if overflowed > 0 {
		// Understated value is only acceptable when visible: say so on
		// every publish cycle while the condition persists.
		t.logger.Warn("emit: tracker over capacity — published in-flight value is an UNDERSTATEMENT",
			"rejected_track_calls", overflowed)
	}

	for _, ck := range toPublish {
		cb := sums[ck] // nil for retiring combos: every bucket zero
		for _, bucket := range AgeBuckets {
			// The combo carries its currency's true exponent — a
			// hardcoded one would misstate zero-exponent currencies.
			var minor, count int64
			if a := cb[bucket]; a != nil {
				minor, count = a.minor, a.count
			}
			money := biz.Money{Amount: minor, Currency: ck.currency, Exponent: ck.exponent}
			t.em.SetInFlight(ck.flow, ck.stage, bucket, money, count)
		}
	}
}

// Start runs Publish on the given cadence until Close. A non-positive
// interval is refused loudly (no silent no-op ticker, no goroutine
// panic), and a second Start is a no-op — one publish loop per tracker.
func (t *InFlightTracker) Start(interval time.Duration) {
	if interval <= 0 {
		t.logger.Warn("emit: tracker Start refused non-positive interval — call Publish yourself or pass a real cadence", "interval", interval)
		return
	}
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		t.logger.Warn("emit: tracker Start called twice; keeping the first loop")
		return
	}
	t.started = true
	t.mu.Unlock()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				t.Publish()
			case <-t.stop:
				return
			}
		}
	}()
}

// Close stops the publish loop (idempotent).
func (t *InFlightTracker) Close() {
	t.stopOnce.Do(func() { close(t.stop) })
	t.wg.Wait()
}
