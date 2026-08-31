// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"hash/maphash"
	"log/slog"
	"sync"
	"sync/atomic"
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
// Age semantics: measured from the first enqueue timestamp — a retry
// re-Track of the same id never makes the backlog look younger.
// trackerShards fixes the shard count. A constant beats deriving one
// from GOMAXPROCS: the semantics never depend on it, 32 idle mutexes
// cost nothing on a small machine, and a deterministic layout is one
// less variable when a storm test fails. Power of two, so the shard
// index is a mask, not a division.
const trackerShards = 32

// trackerShard is one lock's worth of the in-flight set. Track and Done
// contend only within their shard; Publish takes every shard lock at
// once for its snapshot.
type trackerShard struct {
	mu    sync.Mutex
	items map[inflightKey]inflightItem
	// Pad each shard out to a cache line's width: unpadded, four shards
	// share one line and neighbors' lock traffic ping-pongs it. Go only
	// guarantees 8-byte alignment for the array, so this bounds the
	// sharing (at most a tail can straddle into the next head) rather
	// than eliminating it.
	_ [64 - (8+8)%64]byte
}

type InFlightTracker struct {
	em     Emitter
	clock  func() time.Time
	logger *slog.Logger

	// The in-flight set, sharded by message-id hash. Ids carry the
	// cardinality, so hashing the id alone spreads real load; a
	// pathological all-one-id stream lands on one shard, which is
	// exactly the old single-mutex behavior, not worse.
	seed   maphash.Seed
	shards [trackerShards]trackerShard

	// combos we have published non-zero values for and must zero once
	// when they empty, so a stalled dashboard never shows stale levels.
	// Only Publish touches it, and publishMu serializes Publish.
	live map[comboKey]struct{}
	// canonical exponent per currency (string -> int8), pinned on first
	// sight via LoadOrStore: a second exponent for the same currency
	// would silently flap one gauge series between incomparable sums.
	exponents sync.Map
	maxItems  int
	itemCount atomic.Int64 // len across shards; the bound stays exact
	overflow  atomic.Int64 // Track calls the bound rejected (retries count each)
	rejected  atomic.Int64 // Track calls rejected for invalid/mismatched money

	// publishMu serializes snapshot and emission: without it an older
	// snapshot can be emitted after a newer one and win under the
	// order-by-At contract.
	publishMu sync.Mutex

	stateMu  sync.Mutex // guards started
	started  bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// shardFor picks the shard for a message id. trackerShards is a power
// of two, so the mask keeps every hash bit that matters and no modulo.
func (t *InFlightTracker) shardFor(id string) *trackerShard {
	return &t.shards[maphash.String(t.seed, id)&(trackerShards-1)]
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
// value then understates, and Overflowed() reports the rejected Track
// calls (a retried message counts once per attempt). Values below 1 are
// replaced by the default, loudly. Go maps retain their high-water
// bucket memory, so this bound is also the worst-case resident footprint
// after an incident-sized backlog drains.
func WithTrackerMaxItems(n int) TrackerOption {
	return func(t *InFlightTracker) { t.maxItems = n }
}

// WithTrackerLogger sets the warning logger for rejected and overflowed
// tracks (default slog.Default).
func WithTrackerLogger(l *slog.Logger) TrackerOption {
	return func(t *InFlightTracker) { t.logger = l }
}

// NewInFlightTracker builds a tracker publishing through em.
func NewInFlightTracker(em Emitter, opts ...TrackerOption) *InFlightTracker {
	t := &InFlightTracker{
		em:       em,
		clock:    time.Now,
		logger:   slog.Default(),
		seed:     maphash.MakeSeed(),
		live:     map[comboKey]struct{}{},
		maxItems: 1 << 20,
		stop:     make(chan struct{}),
	}
	for i := range t.shards {
		t.shards[i].items = map[inflightKey]inflightItem{}
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
// oldest enqueue time seen (retries never rejuvenate backlog; an earlier
// timestamp on a retrack is adopted as better information) and updates
// the money (amounts should not change; last write wins if they do).
// Rejections are loud: invalid money and a currency re-tracked under a
// different exponent are logged and counted (Rejected()).
func (t *InFlightTracker) Track(flow, stage, id string, money biz.Money, enqueuedAt time.Time) {
	if err := money.Validate(); err != nil {
		t.logger.Warn("emit: tracker rejected invalid money — dropped and counted", "error", err)
		t.rejected.Add(1)
		return
	}

	// First sight pins the exponent atomically; every later Track for
	// the currency compares against the winner. As before, the pin
	// happens even when the Track then overflows the bound. Load first:
	// after the first sight this is the always-taken path, and it is
	// cheaper than LoadOrStore re-proving the store cannot happen.
	pinned, loaded := t.exponents.Load(money.Currency)
	if !loaded {
		pinned, loaded = t.exponents.LoadOrStore(money.Currency, money.Exponent)
	}

	if loaded && pinned.(int8) != money.Exponent {
		t.rejected.Add(1)
		t.logger.Warn(
			"emit: tracker rejected mismatched currency exponent — one series must never flap between incomparable sums",
			"currency", money.Currency,
			"pinned", pinned,
			"got", money.Exponent,
		)
		return
	}

	k := inflightKey{flow, stage, id}
	sh := t.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if prev, ok := sh.items[k]; ok {
		if prev.enqueuedAt.Before(enqueuedAt) {
			enqueuedAt = prev.enqueuedAt
		}

		sh.items[k] = inflightItem{money: money, enqueuedAt: enqueuedAt}
		return
	}

	// Reserve, then insert; roll back on over-reserve. The bound stays
	// EXACT under concurrency — the resident-footprint guarantee in
	// WithTrackerMaxItems's contract, not an approximation.
	if t.itemCount.Add(1) > int64(t.maxItems) {
		t.itemCount.Add(-1)
		t.overflow.Add(1)
		return
	}

	sh.items[k] = inflightItem{money: money, enqueuedAt: enqueuedAt}
}

// Done records a message leaving its stage. Unknown ids are a no-op, so
// Done is idempotent (consumer wrappers retry).
func (t *InFlightTracker) Done(flow, stage, id string) {
	k := inflightKey{flow, stage, id}
	sh := t.shardFor(id)
	sh.mu.Lock()
	if _, ok := sh.items[k]; ok {
		delete(sh.items, k)
		t.itemCount.Add(-1)
	}

	sh.mu.Unlock()
}

// Overflowed returns how many Track calls the bound rejected since the
// tracker was built: nonzero means the published gauge understates the
// true in-flight value.
func (t *InFlightTracker) Overflowed() int64 {
	return t.overflow.Load()
}

// Rejected returns how many Track calls were refused for invalid money
// or a mismatched currency exponent.
func (t *InFlightTracker) Rejected() int64 {
	return t.rejected.Load()
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
	// Hold every shard lock for the whole aggregation: value and count
	// must come from ONE consistent snapshot (ADR-0012), so no shard may
	// mutate while another is being read. Lock in index order — Publish
	// is the only multi-shard locker, so any fixed order is deadlock-free.
	for i := range t.shards {
		t.shards[i].mu.Lock()
	}

	sums := map[comboKey]comboBuckets{}
	for i := range t.shards {
		for k, item := range t.shards[i].items {
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
	}

	for i := range t.shards {
		t.shards[i].mu.Unlock()
	}

	// Every combo with items stays live; every live combo without items
	// gets one zeroing pass and retires. live is publishMu-guarded, and
	// we hold publishMu here.
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

	overflowed := t.overflow.Load()
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
		t.logger.Warn(
			"emit: tracker Start refused non-positive interval — call Publish yourself or pass a real cadence",
			"interval", interval,
		)
		return
	}

	t.stateMu.Lock()
	if t.started {
		t.stateMu.Unlock()
		t.logger.Warn("emit: tracker Start called twice; keeping the first loop")
		return
	}

	t.started = true
	t.stateMu.Unlock()
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
