// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package checkout

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
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
	// Aggregate bands so the assertion is seed-robust, not tuned to one
	// lucky seed: a single trough hour is Poisson(~24) and ±40% is only
	// ~2σ (a seed sweep showed ~4.5% of seeds outside it). Summing the
	// deep-night hours across all five weekdays gives a mean of several
	// hundred, where ±25% is many σ.
	sumBand := func(hours []int, name string) {
		var want, got float64
		for _, h := range hours {
			want += curve[h] * 60
			got += float64(hourly[h])
		}

		if got < want*0.75 || got > want*1.25 {
			t.Errorf("%s: got %.0f arrivals, want ~%.0f", name, got, want)
		}
	}
	var trough, peak []int
	for day := 0; day < 5; day++ {
		for h := 0; h < 4; h++ {
			trough = append(trough, day*24+h)
		}

		peak = append(peak, day*24+10)
	}

	sumBand(trough, "weekday night trough (00-04)")
	sumBand(peak, "weekday 10:00 peak")
	// The ordering peak > trough must hold hour-for-hour regardless.
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

			if txn.SettledAt.Before(txn.CapturedAt) || txn.CapturedAt.Before(txn.AuthedAt) ||
				txn.AuthedAt.Before(txn.CreatedAt) {
				t.Fatalf("%s: stage timestamps out of order: %+v", txn.ID, txn)
			}
		case StateCaptured:
			if !txn.SettledAt.IsZero() {
				t.Fatalf("%s captured but has SettledAt: %+v", txn.ID, txn)
			}

			// In flight at window end is the only honest reason.
			if !txn.CapturedAt.Add(time.Duration(res.Config.SettleDelayMin) * time.Minute).
				After(end.Add(-time.Minute)) {
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
	if total == 0 {
		t.Fatal("run produced no transactions — fraction check would pass vacuously on NaN")
	}

	frac := float64(ent) / float64(total)
	if frac < 0.05 || frac > 0.2 {
		t.Fatalf("enterprise fraction %f outside [0.05, 0.2] (%d of %d)", frac, ent, total)
	}
}

func TestConfigValidation(t *testing.T) {
	mustPanic := func(name string, cfg Config) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected panic, got none")
				}
			}()
			Run(cfg)
		})
	}
	base := Config{Seed: 1, Start: mon, End: mon.Add(time.Hour)}

	neg := base
	neg.CaptureDelayMin = -3
	mustPanic("negative delay", neg)

	badFrac := base
	badFrac.EnterpriseFraction = 1.5
	mustPanic("fraction > 1", badFrac)

	// NaN fails == 0, == NoEnterprise AND both halves of the range test, so
	// before the explicit finiteness check it fell through the whole switch
	// and ran the scenario with a not-a-number segment split.
	nanFrac := base
	nanFrac.EnterpriseFraction = math.NaN()
	mustPanic("fraction is NaN", nanFrac)

	infFrac := base
	infFrac.EnterpriseFraction = math.Inf(1)
	mustPanic("fraction is +Inf", infFrac)

	hot := base
	curve := DefaultCurve()
	curve[10] = 1000 // would silently underflow the Knuth sampler
	hot.Curve = &curve
	mustPanic("curve rate beyond the sampler's honest range", hot)

	// A NaN rate is worse than an out-of-range one: it passes a bound
	// written as two comparisons, and if such an hour is ever sampled
	// poisson() spins forever — its lambda <= 0 guard is false for NaN and
	// its p <= l test can never become true. Validation is the only thing
	// standing between an exported Config field and a non-terminating run,
	// which is why this is asserted at applyDefaults and not left to a
	// scenario that happens to reach hour 10.
	nanCurve := DefaultCurve()
	nanCurve[10] = math.NaN()
	nanHot := base
	nanHot.Curve = &nanCurve
	mustPanic("curve rate is NaN", nanHot)

	// Sentinels express literal zeros instead of being coerced to defaults.
	zero := base
	zero.EnterpriseFraction = NoEnterprise
	zero.CaptureDelayMin = InstantStage
	zero.SettleDelayMin = InstantStage
	res := Run(zero)
	if len(res.Ledger.Txns) == 0 {
		t.Fatal("sentinel run produced no transactions")
	}

	for _, txn := range res.Ledger.Txns {
		if txn.Segment == SegmentEnterprise {
			t.Fatalf("NoEnterprise run produced an enterprise txn: %+v", txn)
		}

		if txn.State != StateSettled {
			t.Fatalf("InstantStage run left %s in state %s", txn.ID, txn.State)
		}

		if !txn.SettledAt.Equal(txn.CreatedAt) {
			t.Fatalf("InstantStage txn %s settled at %v, created %v", txn.ID, txn.SettledAt, txn.CreatedAt)
		}
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
