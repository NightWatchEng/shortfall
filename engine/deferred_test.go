package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/examples/checkout"
	"github.com/NightWatchEng/shortfall/query"
	"github.com/NightWatchEng/shortfall/query/memq"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/NightWatchEng/shortfall/testkit"
)

func inflightPoint(stage, bucket, currency string, value int64, at time.Time) emit.MetricPoint {
	return emit.MetricPoint{
		Name:   "biz_inflight_value",
		Labels: map[string]string{"flow": "invoice.pay", "stage": stage, "age_bucket": bucket, "currency": currency},
		Value:  value, At: at,
	}
}

// testRegistry loads the reference registry (capture SLA PT30M -> lost,
// settle SLA P1D -> at_risk).
func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Load("../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return &reg
}

func TestDeferredByBucketAndCurrency(t *testing.T) {
	at := win.To.Add(-time.Minute)
	metrics := []emit.MetricPoint{
		inflightPoint("capture", "5m-30m", "USD", 1000, at),
		inflightPoint("capture", "gt2h", "USD", 500, at),   // breached (>= 30m) -> lost
		inflightPoint("capture", "30m-2h", "EUR", 700, at), // breached -> lost, EUR
		inflightPoint("settle", "gt2h", "USD", 9000, at),   // settle SLA P1D not breached at gt2h(120m)
	}
	q := memq.New(memq.WithMetrics(metrics))
	leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}
	if leg.ByCurrency["USD"] != 10500 || leg.ByCurrency["EUR"] != 700 {
		t.Fatalf("ByCurrency = %v, want USD 10500 EUR 700", leg.ByCurrency)
	}
	if leg.ByAgeBucket["5m-30m"]["USD"] != 1000 || leg.ByAgeBucket["gt2h"]["USD"] != 9500 {
		t.Fatalf("ByAgeBucket = %v", leg.ByAgeBucket)
	}
	// Projected-lost: capture breaches (30m boundary) where on_breach=lost.
	// USD gt2h 500 + EUR 30m-2h 700; settle is at_risk (not lost) and the
	// capture 5m-30m is not past the 30m deadline.
	if leg.ProjectedLostMinor["USD"] != 500 || leg.ProjectedLostMinor["EUR"] != 700 {
		t.Fatalf("ProjectedLostMinor = %v, want USD 500 EUR 700", leg.ProjectedLostMinor)
	}
	if leg.OldestAgeMinutes != 120 {
		t.Fatalf("OldestAgeMinutes = %d, want 120 (gt2h floor)", leg.OldestAgeMinutes)
	}
	if leg.Evidence != EvidenceDeterministic {
		t.Fatalf("evidence = %q", leg.Evidence)
	}
	// Counts are the honest gap.
	if leg.SLABreaches != 0 || leg.Count != 0 || len(leg.Caveats) == 0 {
		t.Fatalf("expected count gap with a caveat, got breaches=%d count=%d caveats=%v", leg.SLABreaches, leg.Count, leg.Caveats)
	}
}

func TestDeferredAtRiskIsNotProjectedLost(t *testing.T) {
	at := win.To.Add(-time.Minute)
	// settle SLA is P1D (1440m) -> at_risk; even a gt2h bucket is neither past
	// the deadline nor a "lost" policy, so projected-lost stays empty.
	metrics := []emit.MetricPoint{inflightPoint("settle", "gt2h", "USD", 9000, at)}
	q := memq.New(memq.WithMetrics(metrics))
	leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(leg.ProjectedLostMinor) != 0 {
		t.Fatalf("at_risk/under-deadline value must not be projected-lost, got %v", leg.ProjectedLostMinor)
	}
	if leg.ByCurrency["USD"] != 9000 {
		t.Fatalf("still counted as deferred value: %v", leg.ByCurrency)
	}
}

func TestDeferredEdgeCases(t *testing.T) {
	at := win.To.Add(-time.Minute)
	t.Run("nil registry: value counted, no projected-lost", func(t *testing.T) {
		q := memq.New(memq.WithMetrics([]emit.MetricPoint{inflightPoint("capture", "gt2h", "USD", 500, at)}))
		leg, err := Deferred(context.Background(), nil, q, Request{Window: win, Flows: []string{"invoice.pay"}})
		if err != nil {
			t.Fatal(err)
		}
		if leg.ByCurrency["USD"] != 500 {
			t.Fatalf("value = %v, want 500", leg.ByCurrency)
		}
		if len(leg.ProjectedLostMinor) != 0 {
			t.Fatalf("nil registry cannot know SLAs; projected-lost must be empty, got %v", leg.ProjectedLostMinor)
		}
	})
	t.Run("no flows: scope-only aggregates all flows", func(t *testing.T) {
		q := memq.New(memq.WithMetrics([]emit.MetricPoint{inflightPoint("capture", "5m-30m", "USD", 300, at)}))
		leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win}) // no Flows
		if err != nil {
			t.Fatal(err)
		}
		if leg.ByCurrency["USD"] != 300 {
			t.Fatalf("scope-only must still read the gauge, got %v", leg.ByCurrency)
		}
	})
	t.Run("zero level does not create a bucket entry", func(t *testing.T) {
		q := memq.New(memq.WithMetrics([]emit.MetricPoint{inflightPoint("capture", "gt2h", "USD", 0, at)}))
		leg, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win, Flows: []string{"invoice.pay"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(leg.ByAgeBucket) != 0 || len(leg.ByCurrency) != 0 {
			t.Fatalf("a zero level must not create entries, got buckets=%v cur=%v", leg.ByAgeBucket, leg.ByCurrency)
		}
	})
}

func TestDeferredNoMetricsErrors(t *testing.T) {
	q := memq.New(memq.WithCaps(query.Caps{Events: true}))
	if _, err := Deferred(context.Background(), testRegistry(t), q, Request{Window: win}); err == nil {
		t.Fatal("deferred without a metric source must error")
	}
}

// TestDeferredMatchesGoldenQueueScenario runs a capture consumer-stall through
// the in-memory querier and checks deferred totals, per-bucket ages, and
// projected-lost against ground truth computed independently from the ledger.
func TestDeferredMatchesGoldenQueueScenario(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) // Monday
	end := start.Add(3 * time.Hour)
	res := checkout.Run(checkout.Config{
		Seed:  11,
		Start: start,
		End:   end,
		Faults: []checkout.FaultSpec{{
			Kind:  checkout.FaultConsumerStall,
			From:  start.Add(30 * time.Minute),
			To:    end, // stall to the end so txns pile up in the capture queue
			Queue: checkout.QueueCapture,
		}},
	})

	// Ground truth computed INDEPENDENTLY of the leg: the deadline and policy
	// are read from the registry (not hardcoded), the bucket floors use a
	// local duration table (not the leg's package map), and breach is the
	// bucket-floor-vs-deadline rule the leg documents.
	reg := testRegistry(t)
	f, ok := reg.Flow("invoice.pay")
	if !ok {
		t.Fatal("registry missing invoice.pay")
	}
	capSLA := f.SLA["capture"]
	capLost := capSLA.OnBreach == registry.BreachLost
	localFloor := map[string]time.Duration{
		"lt1m": 0, "1m-5m": time.Minute, "5m-30m": 5 * time.Minute, "30m-2h": 30 * time.Minute, "gt2h": 120 * time.Minute,
	}
	wantByCur := map[string]int64{}
	wantBucket := map[string]int64{}
	wantProjLost := map[string]int64{}
	for _, tx := range res.Ledger.Txns {
		if tx.State != checkout.StateAuthed || tx.AuthedAt.IsZero() || tx.AuthedAt.After(end) {
			continue
		}
		bucket := emit.AgeBucketFor(end.Sub(tx.AuthedAt))
		wantByCur[tx.Currency] += tx.AmountMinor
		wantBucket[bucket] += tx.AmountMinor
		if capLost && localFloor[bucket] >= capSLA.Deadline {
			wantProjLost[tx.Currency] += tx.AmountMinor
		}
	}
	if wantByCur["USD"] == 0 {
		t.Fatal("consumer-stall produced no in-flight capture value; adjust the scenario")
	}

	q := testkit.QuerierFromResult(res)
	leg, err := Deferred(context.Background(), reg, q,
		Request{Window: query.TimeRange{From: start, To: end.Add(time.Second)}, Flows: []string{"invoice.pay"}})
	if err != nil {
		t.Fatal(err)
	}
	if leg.ByCurrency["USD"] != wantByCur["USD"] {
		t.Fatalf("deferred USD = %d, want %d", leg.ByCurrency["USD"], wantByCur["USD"])
	}
	for bucket, want := range wantBucket {
		got := leg.ByAgeBucket[bucket]["USD"]
		if got != want {
			t.Fatalf("bucket %s USD = %d, want %d", bucket, got, want)
		}
	}
	if leg.ProjectedLostMinor["USD"] != wantProjLost["USD"] {
		t.Fatalf("projected-lost USD = %d, want %d", leg.ProjectedLostMinor["USD"], wantProjLost["USD"])
	}
}
