package emit

import (
	"sync"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// AgeBucketFor maps a message age onto the fixed ADR-0005 buckets.
// Boundaries are half-open on the left: exactly 1m is 1m-5m, exactly 2h
// is gt2h. Negative ages (producer/consumer clock skew) clamp to the
// youngest bucket — skew must never invent old backlog.
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
	em    Emitter
	clock func() time.Time

	mu    sync.Mutex
	items map[inflightKey]inflightItem
	// combos we have published non-zero values for and must zero once
	// when they empty, so a stalled dashboard never shows stale levels.
	live     map[comboKey]struct{}
	maxItems int
	overflow int64

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
// bound, Track calls are dropped and counted — the published value is
// then an UNDERSTATEMENT and Overflowed() says by how many messages.
func WithTrackerMaxItems(n int) TrackerOption {
	return func(t *InFlightTracker) { t.maxItems = n }
}

// NewInFlightTracker builds a tracker publishing through em.
func NewInFlightTracker(em Emitter, opts ...TrackerOption) *InFlightTracker {
	t := &InFlightTracker{
		em:       em,
		clock:    time.Now,
		items:    map[inflightKey]inflightItem{},
		live:     map[comboKey]struct{}{},
		maxItems: 1 << 20,
		stop:     make(chan struct{}),
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Track records a message entering a stage. Re-tracking an id keeps the
// ORIGINAL enqueue time (retries do not rejuvenate backlog) and updates
// the money (amounts should not change; last write wins if they do).
func (t *InFlightTracker) Track(flow, stage, id string, money biz.Money, enqueuedAt time.Time) {
	if money.Validate() != nil {
		return // the emitter would reject it too; nothing to track
	}
	k := inflightKey{flow, stage, id}
	t.mu.Lock()
	defer t.mu.Unlock()
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

// Overflowed returns how many Track calls the bound rejected since the
// tracker was built: nonzero means the published gauge UNDERSTATES the
// true in-flight value.
func (t *InFlightTracker) Overflowed() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overflow
}

// Publish computes the current per-(flow, stage, bucket, currency) sums
// and pushes every bucket of every live combo — including zeroes, so a
// bucket that empties reads 0 instead of holding its last level. A combo
// with no items is zeroed one final time and then retired from
// publishing (no forever-zero churn).
func (t *InFlightTracker) Publish() {
	now := t.clock()

	type comboBuckets map[string]int64 // bucket -> minor units
	t.mu.Lock()
	sums := map[comboKey]comboBuckets{}
	for k, item := range t.items {
		ck := comboKey{k.flow, k.stage, item.money.Currency, item.money.Exponent}
		cb, ok := sums[ck]
		if !ok {
			cb = comboBuckets{}
			sums[ck] = cb
		}
		cb[AgeBucketFor(now.Sub(item.enqueuedAt))] += item.money.Amount
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
	if t.overflow > 0 {
		// Understated value is only acceptable when visible.
		t.overflowWarnLocked()
	}
	t.mu.Unlock()

	for _, ck := range toPublish {
		cb := sums[ck] // nil for retiring combos: every bucket zero
		for _, bucket := range AgeBuckets {
			// The combo carries its currency's true exponent — a
			// hardcoded one would misstate zero-exponent currencies.
			money := biz.Money{Amount: cb[bucket], Currency: ck.currency, Exponent: ck.exponent}
			t.em.SetInFlight(ck.flow, ck.stage, bucket, money)
		}
	}
}

func (t *InFlightTracker) overflowWarnLocked() {
	// The emitter owns logging policy; the tracker's contract is the
	// Overflowed() accessor. This hook exists so a future logger option
	// has one place to land; today the counter is the interface.
}

// Start runs Publish on the given cadence until Close.
func (t *InFlightTracker) Start(interval time.Duration) {
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
