package emit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

func TestAgeBucketFor(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"zero", 0, AgeLt1m},
		{"59s", 59 * time.Second, AgeLt1m},
		{"exactly 1m", time.Minute, Age1mTo5m},
		{"4m59s", 5*time.Minute - time.Second, Age1mTo5m},
		{"exactly 5m", 5 * time.Minute, Age5mTo30m},
		{"29m59s", 30*time.Minute - time.Second, Age5mTo30m},
		{"exactly 30m", 30 * time.Minute, Age30mTo2h},
		{"1h59m59s", 2*time.Hour - time.Second, Age30mTo2h},
		{"exactly 2h", 2 * time.Hour, AgeGt2h},
		{"41m stall shape", 41 * time.Minute, Age30mTo2h},
		{"days", 72 * time.Hour, AgeGt2h},
		{"negative clock skew clamps low", -30 * time.Second, AgeLt1m},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AgeBucketFor(c.age); got != c.want {
				t.Fatalf("AgeBucketFor(%v) = %q, want %q", c.age, got, c.want)
			}
		})
	}
}

// trackerClock is a mutable fake clock shared by tracker tests.
type trackerClock struct{ now time.Time }

func (c *trackerClock) time() time.Time { return c.now }

func newTrackerHarness(t *testing.T) (*InFlightTracker, *Std, *captureExporter, *trackerClock) {
	t.Helper()
	exp := &captureExporter{}
	clk := &trackerClock{now: testClock}
	em, err := New(testRegistry(t), exp, WithClock(clk.time), WithFlushInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	tr := NewInFlightTracker(em, WithTrackerClock(clk.time))
	return tr, em, exp, clk
}

func gaugeTotals(t *testing.T, exp *captureExporter, em *Std) map[string]int64 {
	t.Helper()
	if err := em.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	metrics, _ := exp.snapshot()
	// Last write per full label set wins: gauges are levels.
	latest := map[string]MetricPoint{}
	for _, p := range metrics {
		if p.Name != "biz_inflight_value" {
			continue
		}
		k := p.Labels["flow"] + "|" + p.Labels["stage"] + "|" + p.Labels["age_bucket"] + "|" + p.Labels["currency"]
		latest[k] = p
	}
	out := map[string]int64{}
	for k, p := range latest {
		out[k] = p.Value
	}
	return out
}

func usd(amount int64) biz.Money { return biz.Money{Amount: amount, Currency: "USD", Exponent: 2} }

func TestTrackerBucketsByAge(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	base := clk.now
	tr.Track("invoice.pay", "capture", "m1", usd(100), base.Add(-30*time.Second)) // lt1m
	tr.Track("invoice.pay", "capture", "m2", usd(200), base.Add(-3*time.Minute))  // 1m-5m
	tr.Track("invoice.pay", "capture", "m3", usd(300), base.Add(-10*time.Minute)) // 5m-30m
	tr.Track("invoice.pay", "capture", "m4", usd(400), base.Add(-41*time.Minute)) // 30m-2h
	tr.Track("invoice.pay", "capture", "m5", usd(500), base.Add(-3*time.Hour))    // gt2h
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	want := map[string]int64{
		"invoice.pay|capture|" + AgeLt1m + "|USD":    100,
		"invoice.pay|capture|" + Age1mTo5m + "|USD":  200,
		"invoice.pay|capture|" + Age5mTo30m + "|USD": 300,
		"invoice.pay|capture|" + Age30mTo2h + "|USD": 400,
		"invoice.pay|capture|" + AgeGt2h + "|USD":    500,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("gauge %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func TestTrackerAgesAcrossPublishes(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.Track("invoice.pay", "capture", "m1", usd(100), clk.now)
	tr.Publish()
	// 3 minutes later the same message must have MOVED buckets, and the
	// old bucket must read zero — a gauge that never returns to zero
	// lies to the pager.
	clk.now = clk.now.Add(3 * time.Minute)
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	if got["invoice.pay|capture|"+Age1mTo5m+"|USD"] != 100 {
		t.Fatalf("aged message not in 1m-5m: %v", got)
	}
	if got["invoice.pay|capture|"+AgeLt1m+"|USD"] != 0 {
		t.Fatalf("vacated bucket must be zeroed: %v", got)
	}
}

func TestTrackerDoneRemovesValue(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.Track("invoice.pay", "capture", "m1", usd(100), clk.now)
	tr.Track("invoice.pay", "capture", "m2", usd(250), clk.now)
	tr.Publish()
	tr.Done("invoice.pay", "capture", "m1")
	tr.Done("invoice.pay", "capture", "never-tracked") // idempotent no-op
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	if got["invoice.pay|capture|"+AgeLt1m+"|USD"] != 250 {
		t.Fatalf("after Done: %v", got)
	}
}

func TestTrackerRetrackKeepsOriginalAge(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	enq := clk.now.Add(-10 * time.Minute)
	tr.Track("invoice.pay", "capture", "m1", usd(100), enq)
	// A retry re-tracks the same id "now"; age measures time since FIRST
	// enqueue — a retry does not make the backlog younger.
	tr.Track("invoice.pay", "capture", "m1", usd(100), clk.now)
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	if got["invoice.pay|capture|"+Age5mTo30m+"|USD"] != 100 {
		t.Fatalf("retrack reset the age: %v", got)
	}
	var total int64
	for _, v := range got {
		total += v
	}
	if total != 100 {
		t.Fatalf("retrack double-counted: %v", got)
	}
}

func TestTrackerEmptiedComboZeroesOnceThenStops(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.Track("invoice.pay", "capture", "m1", usd(100), clk.now)
	tr.Publish()
	tr.Done("invoice.pay", "capture", "m1")
	tr.Publish() // must zero every bucket of the emptied combo
	got := gaugeTotals(t, exp, em)
	for _, b := range AgeBuckets {
		if got["invoice.pay|capture|"+b+"|USD"] != 0 {
			t.Fatalf("bucket %s not zeroed after combo emptied: %v", b, got)
		}
	}
	// After the zero-publish the combo retires: the next publish emits
	// nothing new for it (no unbounded forever-zero series churn).
	exp.mu.Lock()
	exp.metrics = nil
	exp.mu.Unlock()
	tr.Publish()
	if err := em.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	metrics, _ := exp.snapshot()
	for _, p := range metrics {
		if p.Name == "biz_inflight_value" {
			t.Fatalf("retired combo still publishing: %v", p)
		}
	}
}

func TestTrackerTenThousandAcrossBoundaries(t *testing.T) {
	// The bead's acceptance: 10k simulated in-flight messages bucketed
	// correctly, boundary values included.
	tr, em, exp, clk := newTrackerHarness(t)
	ages := []time.Duration{0, time.Minute - time.Nanosecond, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 3 * time.Hour}
	wantTotals := map[string]int64{}
	for i := 0; i < 10000; i++ {
		age := ages[i%len(ages)]
		id := fmt.Sprintf("m%05d", i)
		tr.Track("invoice.pay", "capture", id, usd(1), clk.now.Add(-age))
		wantTotals[AgeBucketFor(age)]++
	}
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	for _, b := range AgeBuckets {
		if got["invoice.pay|capture|"+b+"|USD"] != wantTotals[b] {
			t.Fatalf("bucket %s = %d, want %d", b, got["invoice.pay|capture|"+b+"|USD"], wantTotals[b])
		}
	}
}

func TestTrackerCapIsLoud(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.maxItems = 3
	for i := 0; i < 5; i++ {
		tr.Track("invoice.pay", "capture", fmt.Sprintf("m%d", i), usd(1), clk.now)
	}
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	if got["invoice.pay|capture|"+AgeLt1m+"|USD"] != 3 {
		t.Fatalf("cap not applied: %v", got)
	}
	if tr.Overflowed() != 2 {
		t.Fatalf("overflow count %d, want 2 — understated in-flight value must be visible", tr.Overflowed())
	}
}

func BenchmarkAgeBucketFor(b *testing.B) {
	ages := [...]time.Duration{30 * time.Second, 3 * time.Minute, 10 * time.Minute, time.Hour, 3 * time.Hour}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = AgeBucketFor(ages[i%len(ages)])
	}
}

func BenchmarkTrackerPublish10k(b *testing.B) {
	exp := &captureExporter{}
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		b.Fatal(err)
	}
	em, err := New(&reg, exp, WithFlushInterval(0), WithBufferSize(1<<20))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = em.Close(context.Background()) }()
	tr := NewInFlightTracker(em)
	now := time.Now()
	for i := 0; i < 10000; i++ {
		tr.Track("invoice.pay", "capture", fmt.Sprintf("m%05d", i), usd(1), now.Add(-time.Duration(i)*time.Second))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Publish()
	}
}

func TestTrackerPreservesCurrencyExponent(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.Track("invoice.pay", "capture", "jp1", biz.Money{Amount: 14900, Currency: "JPY", Exponent: 0}, clk.now)
	tr.Publish()
	if err := em.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	metrics, _ := exp.snapshot()
	for _, p := range metrics {
		if p.Name == "biz_inflight_value" && p.Labels["currency"] == "JPY" && p.Labels["age_bucket"] == AgeLt1m {
			if p.Value != 14900 {
				t.Fatalf("JPY gauge %d", p.Value)
			}
			return
		}
	}
	t.Fatal("JPY gauge not published")
}

func TestTrackerRejectionsAreLoud(t *testing.T) {
	tr, _, _, clk := newTrackerHarness(t)
	cases := []struct {
		name  string
		money biz.Money
	}{
		{"invalid money", biz.Money{Amount: -5, Currency: "USD", Exponent: 2}},
		{"mismatched exponent", biz.Money{Amount: 100, Currency: "USD", Exponent: 0}},
	}
	tr.Track("invoice.pay", "capture", "pin", usd(1), clk.now) // pins USD at exponent 2
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := tr.Rejected()
			tr.Track("invoice.pay", "capture", "x-"+c.name, c.money, clk.now)
			if tr.Rejected() != before+1 {
				t.Fatalf("rejection not counted for %s", c.name)
			}
		})
	}
}

func TestTrackerRetireThenReappear(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.Track("invoice.pay", "capture", "m1", usd(100), clk.now)
	tr.Publish()
	tr.Done("invoice.pay", "capture", "m1")
	tr.Publish() // zero + retire
	exp.mu.Lock()
	exp.metrics = nil
	exp.mu.Unlock()
	tr.Track("invoice.pay", "capture", "m2", usd(700), clk.now)
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	if got["invoice.pay|capture|"+AgeLt1m+"|USD"] != 700 {
		t.Fatalf("retired combo did not resume publishing: %v", got)
	}
}

func TestTrackerCurrencyChangeRetiresOldCombo(t *testing.T) {
	tr, em, exp, clk := newTrackerHarness(t)
	tr.Track("invoice.pay", "capture", "m1", usd(100), clk.now)
	tr.Publish()
	// The message is re-tracked under a different currency (caller data
	// fix): the USD combo must zero out, JPY must appear.
	tr.Track("invoice.pay", "capture", "m1", biz.Money{Amount: 9000, Currency: "JPY", Exponent: 0}, clk.now)
	tr.Publish()
	got := gaugeTotals(t, exp, em)
	if got["invoice.pay|capture|"+AgeLt1m+"|USD"] != 0 {
		t.Fatalf("vacated USD combo not zeroed: %v", got)
	}
	if got["invoice.pay|capture|"+AgeLt1m+"|JPY"] != 9000 {
		t.Fatalf("JPY combo missing: %v", got)
	}
}

func TestTrackerStartCloseLifecycle(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp, WithFlushInterval(2*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	tr := NewInFlightTracker(em)
	tr.Start(0)                    // refused loudly, must not panic or spin
	tr.Start(2 * time.Millisecond) // real loop
	tr.Start(2 * time.Millisecond) // second Start: no-op
	tr.Track("invoice.pay", "capture", "m1", usd(123), time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		metrics, _ := exp.snapshot()
		for _, p := range metrics {
			if p.Name == "biz_inflight_value" && p.Value == 123 {
				tr.Close()
				tr.Close() // idempotent
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("publish loop never delivered")
}

func TestTrackerMaxItemsFloor(t *testing.T) {
	exp := &captureExporter{}
	em, err := New(testRegistry(t), exp, WithFlushInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	cases := []struct {
		name string
		n    int
	}{{"zero", 0}, {"negative", -5}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewInFlightTracker(em, WithTrackerMaxItems(c.n))
			tr.Track("invoice.pay", "capture", "m1", usd(1), time.Now())
			if tr.Overflowed() != 0 {
				t.Fatal("a nonsense bound silently disabled tracking — must fall back to the default loudly")
			}
		})
	}
}
