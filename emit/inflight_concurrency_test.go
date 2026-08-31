package emit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// These tests pin the tracker semantics that must survive any change to
// its internal locking: the exact maxItems bound, single-winner exponent
// pinning, and publish reconciliation, each under goroutine storms. They
// were written against the single-mutex tracker and watched pass there
// before the sharded one existed, so a regression here is a semantic
// break, not a new expectation. Run with -race to make them bite.

// sumEmitter aggregates SetInFlight calls per (flow, stage, currency,
// exponent) so a test can reconcile published value/count totals against
// ground truth without standing up the full Std emitter.
type sumEmitter struct {
	mu     sync.Mutex
	minor  map[string]int64
	counts map[string]int64
}

func newSumEmitter() *sumEmitter {
	return &sumEmitter{minor: map[string]int64{}, counts: map[string]int64{}}
}

func (s *sumEmitter) Record(_ context.Context, _ string, _ biz.Result, _ ...Option) {}

func (s *sumEmitter) RecordProviderCall(string, string, string) {}

func (s *sumEmitter) SetInFlight(flow, stage, _ string, money biz.Money, count int64) {
	k := fmt.Sprintf("%s|%s|%s|%d", flow, stage, money.Currency, money.Exponent)
	s.mu.Lock()
	s.minor[k] += money.Amount
	s.counts[k] += count
	s.mu.Unlock()
}

func (s *sumEmitter) totals() (minor, count int64, exponents map[int8]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exponents = map[int8]bool{}
	for k, v := range s.minor {
		minor += v
		var exp int8
		if _, err := fmt.Sscanf(k[len(k)-1:], "%d", &exp); err == nil {
			exponents[exp] = true
		}
	}
	for _, v := range s.counts {
		count += v
	}
	return minor, count, exponents
}

func TestTrackerBoundExactUnderConcurrentTracks(t *testing.T) {
	const bound = 64
	const workers = 8
	const perWorker = 100 // 800 distinct ids against a bound of 64
	em := newSumEmitter()
	tr := NewInFlightTracker(em, WithTrackerLogger(quietLogger()), WithTrackerMaxItems(bound))
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tr.Track("invoice.pay", "capture", fmt.Sprintf("w%d-m%d", w, i), usd(1), now)
			}
		}(w)
	}
	wg.Wait()

	// The bound is documented as the worst-case resident footprint, so it
	// is exact: accepted + overflowed must account for every call, and
	// accepted must be the bound itself, never one over.
	if got := tr.Overflowed(); got != workers*perWorker-bound {
		t.Fatalf("Overflowed() = %d, want %d", got, workers*perWorker-bound)
	}
	tr.Publish()
	minor, count, _ := em.totals()
	if count != bound {
		t.Fatalf("published in-flight count = %d, want exactly the bound %d", count, bound)
	}
	if minor != bound {
		t.Fatalf("published in-flight minor units = %d, want %d", minor, bound)
	}
}

func TestTrackerExponentPinRaceHasOneWinner(t *testing.T) {
	const workers = 16
	const perWorker = 50
	em := newSumEmitter()
	tr := NewInFlightTracker(em, WithTrackerLogger(quietLogger()))
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			exp := int8(2 + w%2) // half race exponent 2, half exponent 3
			for i := 0; i < perWorker; i++ {
				tr.Track("invoice.pay", "capture", fmt.Sprintf("w%d-m%d", w, i),
					biz.Money{Amount: 1, Currency: "USD", Exponent: exp}, now)
			}
		}(w)
	}
	wg.Wait()

	// Whichever exponent was seen first is pinned; every Track carrying
	// the other must have been rejected loudly. One gauge series never
	// flaps between incomparable sums.
	tr.Publish()
	_, count, exponents := em.totals()
	if len(exponents) != 1 {
		t.Fatalf("published %d distinct exponents for one currency, want 1: %v", len(exponents), exponents)
	}
	if got := tr.Rejected(); got != workers*perWorker-count {
		t.Fatalf("Rejected() = %d, published count = %d; together they must account for all %d tracks",
			got, count, workers*perWorker)
	}
	if count != workers*perWorker/2 {
		t.Fatalf("published count = %d, want %d (exactly the winning half)", count, workers*perWorker/2)
	}
}

func TestTrackerStormReconciles(t *testing.T) {
	const workers = 8
	const paired = 200 // Track+Done pairs per worker: must net to zero
	const held = 25    // ids per worker left in flight: must all publish
	em := newSumEmitter()
	tr := NewInFlightTracker(em, WithTrackerLogger(quietLogger()))
	now := time.Now()

	var wg, pub sync.WaitGroup
	stopPublishing := make(chan struct{})
	pub.Add(1)
	go func() { // a publisher racing the storm, as Start would run one
		defer pub.Done()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopPublishing:
				return
			case <-tick.C:
				tr.Publish()
			}
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < paired; i++ {
				id := fmt.Sprintf("w%d-pair-%d", w, i)
				tr.Track("invoice.pay", "capture", id, usd(3), now)
				tr.Done("invoice.pay", "capture", id)
			}
			for i := 0; i < held; i++ {
				tr.Track("invoice.pay", "capture", fmt.Sprintf("w%d-held-%d", w, i), usd(7), now)
			}
		}(w)
	}
	wg.Wait()
	close(stopPublishing)
	pub.Wait()

	if tr.Overflowed() != 0 || tr.Rejected() != 0 {
		t.Fatalf("overflowed=%d rejected=%d: the storm was not the accept path", tr.Overflowed(), tr.Rejected())
	}
	// Quiesced, the final publish must report exactly the held set: no
	// pair leaked into the in-flight sums, no held id lost or doubled.
	fresh := newSumEmitter()
	tr.em = fresh
	tr.Publish()
	minor, count, _ := fresh.totals()
	if count != workers*held {
		t.Fatalf("final in-flight count = %d, want %d", count, workers*held)
	}
	if minor != int64(workers*held*7) {
		t.Fatalf("final in-flight minor units = %d, want %d", minor, workers*held*7)
	}
}
