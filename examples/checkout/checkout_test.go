package checkout

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// mon is a Monday 00:00 UTC anchor so hour-of-week math is legible in tests.
var mon = time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

func ledgerDigest(t *testing.T, l Ledger) string {
	t.Helper()
	b, err := json.Marshal(l.Txns)
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func TestRunIsDeterministicUnderSeed(t *testing.T) {
	cfg := Config{Seed: 42, Start: mon, End: mon.Add(48 * time.Hour)}
	a := Run(cfg)
	b := Run(cfg)
	if len(a.Ledger.Txns) == 0 {
		t.Fatal("simulation produced no transactions")
	}
	if da, db := ledgerDigest(t, a.Ledger), ledgerDigest(t, b.Ledger); da != db {
		t.Fatalf("same seed produced different ledgers: %s vs %s", da, db)
	}
}

func TestDifferentSeedsDiverge(t *testing.T) {
	cfg := Config{Seed: 1, Start: mon, End: mon.Add(24 * time.Hour)}
	a := Run(cfg)
	cfg.Seed = 2
	b := Run(cfg)
	if ledgerDigest(t, a.Ledger) == ledgerDigest(t, b.Ledger) {
		t.Fatal("different seeds produced identical ledgers")
	}
}

func TestTrafficFollowsCurve(t *testing.T) {
	// One full week; hourly arrival counts should track the curve within
	// Poisson noise. Compare the busiest and quietest weekday hours.
	cfg := Config{Seed: 7, Start: mon, End: mon.Add(7 * 24 * time.Hour)}
	res := Run(cfg)

	hourly := map[int]int{}
	for _, txn := range res.Ledger.Txns {
		hourly[hourOfWeek(txn.CreatedAt)]++
	}

	curve := DefaultCurve()
	// Expected arrivals for an hour = rate/min * 60.
	for _, h := range []int{10 /* Mon 10:00 peak */, 2 /* Mon 02:00 trough */} {
		want := curve[h] * 60
		got := float64(hourly[h])
		// 40% tolerance: a single hour of Poisson arrivals is noisy, but a
		// peak/trough mix-up is 10x and cannot hide in it.
		if got < want*0.6 || got > want*1.4 {
			t.Errorf("hour %d: got %.0f arrivals, want ~%.0f (curve rate %.2f/min)", h, got, want, curve[h])
		}
	}
	// The ordering peak > trough must hold regardless of tolerance.
	if hourly[10] <= hourly[2] {
		t.Errorf("peak hour (%d txns) not busier than trough hour (%d txns)", hourly[10], hourly[2])
	}
}

func TestLifecycleCoherence(t *testing.T) {
	cfg := Config{Seed: 99, Start: mon, End: mon.Add(24 * time.Hour)}
	res := Run(cfg)
	end := cfg.End.Truncate(time.Minute)

	states := map[State]int{}
	for _, txn := range res.Ledger.Txns {
		states[txn.State]++
		if txn.AmountMinor <= 0 {
			t.Fatalf("%s: non-positive amount %d", txn.ID, txn.AmountMinor)
		}
		if txn.Currency != "USD" {
			t.Fatalf("%s: unexpected currency %q", txn.ID, txn.Currency)
		}
		switch txn.State {
		case StateSettled:
			if txn.SettledAt.IsZero() || txn.CapturedAt.IsZero() || txn.AuthedAt.IsZero() {
				t.Fatalf("%s settled with missing stage timestamps: %+v", txn.ID, txn)
			}
			if txn.SettledAt.Before(txn.CapturedAt) || txn.CapturedAt.Before(txn.AuthedAt) || txn.AuthedAt.Before(txn.CreatedAt) {
				t.Fatalf("%s: stage timestamps out of order: %+v", txn.ID, txn)
			}
		case StateCaptured:
			if !txn.SettledAt.IsZero() {
				t.Fatalf("%s captured but has SettledAt: %+v", txn.ID, txn)
			}
			// In flight at window end is the only honest reason.
			if !txn.CapturedAt.Add(time.Duration(res.Config.SettleDelayMin) * time.Minute).After(end.Add(-time.Minute)) {
				t.Fatalf("%s: captured txn should have settled inside the window: %+v", txn.ID, txn)
			}
		case StateAuthed:
			if !txn.CapturedAt.IsZero() {
				t.Fatalf("%s authed but has CapturedAt: %+v", txn.ID, txn)
			}
		default:
			t.Fatalf("%s: unexpected state %q in fault-free run", txn.ID, txn.State)
		}
	}
	if states[StateSettled] == 0 {
		t.Fatal("fault-free run settled nothing")
	}
	// The in-flight tail must be small relative to a full day.
	if tail := states[StateAuthed] + states[StateCaptured]; tail > states[StateSettled]/10 {
		t.Fatalf("in-flight tail suspiciously large: %d in flight vs %d settled", tail, states[StateSettled])
	}
}

func TestSegmentsAndAmounts(t *testing.T) {
	cfg := Config{Seed: 5, Start: mon, End: mon.Add(48 * time.Hour)}
	res := Run(cfg)
	var smb, ent int
	for _, txn := range res.Ledger.Txns {
		switch txn.Segment {
		case SegmentSMB:
			smb++
			if txn.AmountMinor < 4200 || txn.AmountMinor >= 24200 {
				t.Fatalf("smb amount out of range: %d", txn.AmountMinor)
			}
		case SegmentEnterprise:
			ent++
			if txn.AmountMinor < 31000 || txn.AmountMinor >= 151000 {
				t.Fatalf("enterprise amount out of range: %d", txn.AmountMinor)
			}
		default:
			t.Fatalf("unknown segment %q", txn.Segment)
		}
	}
	total := smb + ent
	frac := float64(ent) / float64(total)
	if frac < 0.05 || frac > 0.2 {
		t.Fatalf("enterprise fraction %f outside [0.05, 0.2] (%d of %d)", frac, ent, total)
	}
}

func BenchmarkRunDay(b *testing.B) {
	cfg := Config{Seed: 42, Start: mon, End: mon.Add(24 * time.Hour)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res := Run(cfg)
		if len(res.Ledger.Txns) == 0 {
			b.Fatal("no txns")
		}
	}
}
