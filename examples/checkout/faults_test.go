package checkout

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func day2(h, m int) time.Time {
	return mon.Add(24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func TestAPI5xxFault(t *testing.T) {
	cfg := Config{
		Seed: 11, Start: mon, End: mon.Add(48 * time.Hour),
		Faults: []FaultSpec{{Kind: FaultAPI5xx, Rate: 0.5, From: day2(14, 0), To: day2(15, 0)}},
	}
	res := Run(cfg)

	var inWindow, failed, outsideFailed int
	for _, txn := range res.Ledger.Txns {
		if txn.State == StateAuthFail {
			if txn.CreatedAt.Before(day2(14, 0)) || !txn.CreatedAt.Before(day2(15, 0)) {
				outsideFailed++
			}
			failed++
			if !txn.AuthedAt.IsZero() || !txn.CapturedAt.IsZero() {
				t.Fatalf("%s failed auth but has later timestamps: %+v", txn.ID, txn)
			}
		}
		if !txn.CreatedAt.Before(day2(14, 0)) && txn.CreatedAt.Before(day2(15, 0)) {
			inWindow++
		}
	}
	if outsideFailed != 0 {
		t.Fatalf("%d auth failures outside the fault window", outsideFailed)
	}
	if failed == 0 {
		t.Fatal("no auth failures during a 50% 5xx hour")
	}
	// ~50% of in-window arrivals should fail; wide band, mean is ~150.
	frac := float64(failed) / float64(inWindow)
	if frac < 0.35 || frac > 0.65 {
		t.Fatalf("failure fraction %.2f outside [0.35, 0.65] (%d of %d)", frac, failed, inWindow)
	}
}

func TestAbandonmentFault(t *testing.T) {
	cfg := Config{
		Seed: 12, Start: mon, End: mon.Add(48 * time.Hour),
		Faults: []FaultSpec{{Kind: FaultAPILatency, Rate: 0.4, From: day2(9, 0), To: day2(10, 0)}},
	}
	res := Run(cfg)
	var abandoned int
	for _, txn := range res.Ledger.Txns {
		if txn.State == StateAbandoned {
			abandoned++
			if !txn.AuthedAt.IsZero() {
				t.Fatalf("%s abandoned but authed: %+v", txn.ID, txn)
			}
			if txn.CreatedAt.Before(day2(9, 0)) || !txn.CreatedAt.Before(day2(10, 0)) {
				t.Fatalf("%s abandoned outside the fault window", txn.ID)
			}
		}
	}
	if abandoned == 0 {
		t.Fatal("no abandonment during a 40% latency hour")
	}
}

func TestConsumerStallFormsAndDrainsBacklog(t *testing.T) {
	stallFrom, stallTo := day2(14, 2), day2(14, 47)
	cfg := Config{
		Seed: 13, Start: mon, End: mon.Add(48 * time.Hour),
		Faults: []FaultSpec{{Kind: FaultConsumerStall, Queue: QueueCapture, From: stallFrom, To: stallTo}},
	}
	res := Run(cfg)

	var duringStallCaptures, delayed int
	var maxLagMin float64
	for _, txn := range res.Ledger.Txns {
		if txn.CapturedAt.IsZero() {
			continue
		}
		// Strictly inside the window: a completion timestamp equal to the
		// stall start marks work that finished at the boundary — its
		// service minutes were all un-stalled.
		if txn.CapturedAt.After(stallFrom) && txn.CapturedAt.Before(stallTo) {
			duringStallCaptures++
		}
		lag := txn.CapturedAt.Sub(txn.AuthedAt).Minutes()
		if lag > float64(res.Config.CaptureDelayMin) {
			delayed++
			if lag > maxLagMin {
				maxLagMin = lag
			}
		}
	}
	if duringStallCaptures != 0 {
		t.Fatalf("%d captures completed during the stall window", duringStallCaptures)
	}
	if delayed == 0 {
		t.Fatal("stall produced no delayed captures")
	}
	// A txn authed at stall start waits ~45m + drain time.
	if maxLagMin < 40 {
		t.Fatalf("max capture lag %.0fm implausibly small for a 45m stall", maxLagMin)
	}
	// Everything eventually settles: the backlog drains at capacity after
	// the stall, well before the window ends the next day.
	for _, txn := range res.Ledger.Txns {
		if txn.CreatedAt.Before(day2(20, 0)) && txn.State != StateSettled {
			t.Fatalf("%s (created %v) still %s long after the stall drained", txn.ID, txn.CreatedAt, txn.State)
		}
	}
}

func TestBlackoutSuppressesAndRecovers(t *testing.T) {
	from, to := day2(10, 0), day2(11, 0)
	cfg := Config{
		Seed: 14, Start: mon, End: mon.Add(48 * time.Hour),
		Faults: []FaultSpec{{
			Kind: FaultBlackout, From: from, To: to,
			RecoveredFraction: 0.6, RecoveryWithin: 2 * time.Hour,
		}},
	}
	res := Run(cfg)

	var suppressedTotal int
	for _, s := range res.Suppressed {
		if s.Minute.Before(from) || !s.Minute.Before(to) {
			t.Fatalf("suppressed minute %v outside the blackout window", s.Minute)
		}
		suppressedTotal += s.Count
	}
	if suppressedTotal == 0 {
		t.Fatal("blackout suppressed nothing during a peak hour")
	}

	var arrivedInWindow, recovered int
	for _, txn := range res.Ledger.Txns {
		if !txn.CreatedAt.Before(from) && txn.CreatedAt.Before(to) {
			arrivedInWindow++
		}
		if txn.Recovered {
			recovered++
			if txn.CreatedAt.Before(to) || txn.CreatedAt.After(to.Add(2*time.Hour)) {
				t.Fatalf("recovered txn %s arrived at %v, outside the recovery window", txn.ID, txn.CreatedAt)
			}
		}
	}
	if arrivedInWindow != 0 {
		t.Fatalf("%d transactions arrived during a total blackout", arrivedInWindow)
	}
	frac := float64(recovered) / float64(suppressedTotal)
	if frac < 0.45 || frac > 0.75 {
		t.Fatalf("recovered fraction %.2f outside [0.45, 0.75] (%d of %d)", frac, recovered, suppressedTotal)
	}
}

func TestFaultValidation(t *testing.T) {
	base := Config{Seed: 1, Start: mon, End: mon.Add(time.Hour)}
	cases := []struct {
		name string
		f    FaultSpec
	}{
		{"rate required", FaultSpec{Kind: FaultAPI5xx, Rate: 0, From: mon, To: mon.Add(time.Minute)}},
		{"rate above one", FaultSpec{Kind: FaultAPI5xx, Rate: 1.5, From: mon, To: mon.Add(time.Minute)}},
		{"bad queue", FaultSpec{Kind: FaultConsumerStall, Queue: "nope", From: mon, To: mon.Add(time.Minute)}},
		{"missing within", FaultSpec{Kind: FaultBlackout, RecoveredFraction: 0.5, From: mon, To: mon.Add(time.Minute)}},
		{"unknown kind", FaultSpec{Kind: "gremlins", From: mon, To: mon.Add(time.Minute)}},
		{"inverted window", FaultSpec{Kind: FaultAPI5xx, Rate: 0.5, From: mon.Add(time.Minute), To: mon}},
		// A range written as a pair of comparisons admits NaN, which fails
		// both halves — that is what the first two pin. +Inf is caught by
		// the bound either way; its row is the only test in the tree
		// asserting the blackout fraction bound at all, so it stays.
		{"nan rate", FaultSpec{Kind: FaultAPI5xx, Rate: math.NaN(), From: mon, To: mon.Add(time.Minute)}},
		{"nan recovered fraction", FaultSpec{Kind: FaultBlackout, RecoveredFraction: math.NaN(), From: mon, To: mon.Add(time.Minute)}},
		{"inf recovered fraction", FaultSpec{Kind: FaultBlackout, RecoveredFraction: math.Inf(1), From: mon, To: mon.Add(time.Minute)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			cfg.Faults = []FaultSpec{c.f}
			defer func() {
				if recover() == nil {
					t.Errorf("%+v: expected panic", c.f)
				}
			}()
			Run(cfg)
		})
	}
}

func TestScenarioFileExpressesTheCanonicalStall(t *testing.T) {
	// A scenario file must be able to express "14:02-14:47 consumer
	// stall".
	yamlDoc := `
name: capture-stall
seed: 42
start: 2026-08-24T00:00:00Z
end: 2026-08-26T00:00:00Z
faults:
  - kind: queue-consumer-stall
    queue: capture
    from: 2026-08-25T14:02:00Z
    to: 2026-08-25T14:47:00Z
`
	path := filepath.Join(t.TempDir(), "stall.yaml")
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := LoadScenario(path)
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if sc.Name != "capture-stall" || len(sc.Config.Faults) != 1 {
		t.Fatalf("unexpected scenario: %+v", sc)
	}
	f := sc.Config.Faults[0]
	if f.Kind != FaultConsumerStall || f.Queue != QueueCapture {
		t.Fatalf("unexpected fault: %+v", f)
	}
	if got := f.To.Sub(f.From); got != 45*time.Minute {
		t.Fatalf("stall length %v, want 45m", got)
	}
	// And it runs.
	res := Run(sc.Config)
	if len(res.Ledger.Txns) == 0 {
		t.Fatal("scenario run produced no transactions")
	}
}

func TestScenarioParseRejections(t *testing.T) {
	cases := map[string]string{
		"unknown field": "name: x\nseed: 1\nstart: 2026-08-24T00:00:00Z\nend: 2026-08-25T00:00:00Z\nbogus_knob: 3\n",
		"missing name":  "seed: 1\nstart: 2026-08-24T00:00:00Z\nend: 2026-08-25T00:00:00Z\n",
		"fault outside window": `name: x
seed: 1
start: 2026-08-24T00:00:00Z
end: 2026-08-25T00:00:00Z
faults:
  - kind: api-5xx
    rate: 0.5
    from: 2026-08-25T01:00:00Z
    to: 2026-08-25T02:00:00Z
`,
		"bad duration": `name: x
seed: 1
start: 2026-08-24T00:00:00Z
end: 2026-08-25T00:00:00Z
faults:
  - kind: upstream-blackout
    from: 2026-08-24T01:00:00Z
    to: 2026-08-24T02:00:00Z
    recovered_fraction: 0.5
    recovery_within: soonish
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseScenario([]byte(doc)); err == nil {
				t.Error("expected error, got none")
			}
		})
	}
}

func TestCommittedScenariosAllLoadAndRun(t *testing.T) {
	matches, err := filepath.Glob("scenarios/*.yaml")
	if err != nil || len(matches) < 3 {
		t.Fatalf("expected the three canonical scenarios, got %v (err %v)", matches, err)
	}
	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			sc, err := LoadScenario(path)
			if err != nil {
				t.Fatal(err)
			}
			if res := Run(sc.Config); len(res.Ledger.Txns) == 0 {
				t.Fatal("run produced no transactions")
			}
		})
	}
}

func TestOverlappingStallsFreezeByUnion(t *testing.T) {
	// Two overlapping stall specs model overlapping causes; the outage is
	// their union [14:00, 14:45), never double-counted. Eligible 13:58
	// with a 5m delay completes at 14:48: 2 un-stalled minutes, freeze
	// across the 45m union, 3 remaining.
	f1 := FaultSpec{Kind: FaultConsumerStall, Queue: QueueCapture, From: day2(14, 0), To: day2(14, 30)}
	f2 := FaultSpec{Kind: FaultConsumerStall, Queue: QueueCapture, From: day2(14, 15), To: day2(14, 45)}
	st := newStage(5, 20, QueueCapture, []FaultSpec{f2, f1}) // order-independent
	got := st.schedule(day2(13, 58))
	if want := day2(14, 48); !got.Equal(want) {
		t.Fatalf("overlapping stalls: completion %v, want %v", got, want)
	}
}

func TestScheduleAssertsEligibilityOrder(t *testing.T) {
	st := newStage(2, 20, QueueCapture, nil)
	st.schedule(day2(10, 0))
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on regressing eligibility")
		}
	}()
	st.schedule(day2(9, 0))
}

func TestRearrivalIntoLaterBlackoutIsSuppressed(t *testing.T) {
	// Recovery from blackout one lands inside blackout two: it must be
	// suppressed there (and rolled under blackout two's model), never
	// processed as a Recovered arrival inside a blackout window.
	b1 := FaultSpec{
		Kind:              FaultBlackout,
		From:              day2(10, 0),
		To:                day2(10, 30),
		RecoveredFraction: 1.0,
		RecoveryWithin:    30 * time.Minute,
	}
	b2 := FaultSpec{Kind: FaultBlackout, From: day2(10, 30), To: day2(11, 30)}
	cfg := Config{Seed: 21, Start: mon, End: mon.Add(48 * time.Hour), Faults: []FaultSpec{b1, b2}}
	res := Run(cfg)
	for _, txn := range res.Ledger.Txns {
		for _, b := range []FaultSpec{b1, b2} {
			if !txn.CreatedAt.Before(b.From) && txn.CreatedAt.Before(b.To) {
				t.Fatalf("txn %s (recovered=%v) created at %v inside blackout [%v, %v)",
					txn.ID, txn.Recovered, txn.CreatedAt, b.From, b.To)
			}
		}
	}
}

func TestOverlappingBlackoutsRejected(t *testing.T) {
	cfg := Config{Seed: 1, Start: mon, End: mon.Add(48 * time.Hour), Faults: []FaultSpec{
		{Kind: FaultBlackout, From: day2(10, 0), To: day2(11, 0)},
		{Kind: FaultBlackout, From: day2(10, 30), To: day2(11, 30)},
	}}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on overlapping blackouts")
		}
	}()
	Run(cfg)
}

func TestUnalignedFaultWindowRejected(t *testing.T) {
	f := FaultSpec{Kind: FaultAPI5xx, Rate: 0.5, From: day2(14, 0).Add(30 * time.Second), To: day2(15, 0)}
	if err := f.Validate(); err == nil {
		t.Fatal("expected minute-alignment rejection")
	}
}

func TestScenarioRejectsExplicitZeroCapacity(t *testing.T) {
	doc := `name: x
seed: 1
start: 2026-08-24T00:00:00Z
end: 2026-08-25T00:00:00Z
capture_capacity_per_min: 0
`
	if _, err := ParseScenario([]byte(doc)); err == nil {
		t.Fatal("explicit zero capacity must be rejected with guidance")
	}
}

func TestGoldenBlockRejections(t *testing.T) {
	mk := func(golden string) string {
		return "name: x\nseed: 1\nstart: 2026-08-24T00:00:00Z\nend: 2026-08-25T00:00:00Z\ngolden:\n" + golden
	}
	cases := map[string]string{
		"inverted":       mk("  from: 2026-08-24T10:00:00Z\n  to: 2026-08-24T09:00:00Z\n  capture_sla: 30m\n"),
		"outside run":    mk("  from: 2026-08-24T10:00:00Z\n  to: 2026-08-25T01:00:00Z\n  capture_sla: 30m\n"),
		"bad sla":        mk("  from: 2026-08-24T10:00:00Z\n  to: 2026-08-24T11:00:00Z\n  capture_sla: soonish\n"),
		"zero sla":       mk("  from: 2026-08-24T10:00:00Z\n  to: 2026-08-24T11:00:00Z\n  capture_sla: 0m\n"),
		"unaligned from": mk("  from: 2026-08-24T10:00:30Z\n  to: 2026-08-24T11:00:00Z\n  capture_sla: 30m\n"),
		"unaligned to":   mk("  from: 2026-08-24T10:00:00Z\n  to: 2026-08-24T10:59:30Z\n  capture_sla: 30m\n"),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseScenario([]byte(doc)); err == nil {
				t.Error("expected error, got none")
			}
		})
	}
	ok := mk("  from: 2026-08-24T10:00:00Z\n  to: 2026-08-24T11:00:00Z\n  capture_sla: 30m\n")
	sc, err := ParseScenario([]byte(ok))
	if err != nil {
		t.Fatalf("valid golden block rejected: %v", err)
	}
	if sc.Golden.CaptureSLA != 30*time.Minute {
		t.Fatalf("golden block not carried: %+v", sc.Golden)
	}
}

func TestRecoveryAttributionSurvivesResuppression(t *testing.T) {
	// Demand suppressed by blackout A, whose recovery lands inside
	// blackout B, keeps A's attribution when it finally arrives.
	a := FaultSpec{
		Kind:              FaultBlackout,
		From:              day2(10, 0),
		To:                day2(10, 10),
		RecoveredFraction: 1.0,
		RecoveryWithin:    time.Minute,
	}
	b := FaultSpec{
		Kind:              FaultBlackout,
		From:              day2(10, 10),
		To:                day2(10, 40),
		RecoveredFraction: 1.0,
		RecoveryWithin:    30 * time.Minute,
	}
	cfg := Config{Seed: 33, Start: mon, End: mon.Add(48 * time.Hour), Faults: []FaultSpec{a, b}}
	res := Run(cfg)

	fromA, fromB := 0, 0
	for _, txn := range res.Ledger.Txns {
		if !txn.Recovered {
			continue
		}
		switch {
		case txn.RecoveredFrom.Equal(a.From):
			fromA++
		case txn.RecoveredFrom.Equal(b.From):
			fromB++
		default:
			t.Fatalf("recovered txn %s has unknown attribution %v", txn.ID, txn.RecoveredFrom)
		}
	}
	if fromA == 0 {
		t.Fatal("no recoveries kept blackout A's attribution through re-suppression")
	}
	if fromB == 0 {
		t.Fatal("blackout B produced no attributed recoveries of its own")
	}
	// Fresh-demand-only suppression accounting: per-minute counts during
	// B must not include A's re-suppressed re-arrivals beyond the fresh
	// arrival rate's plausible ceiling. Structural check instead: total
	// suppressed equals fresh arrivals suppressed (recoveries excluded),
	// so suppressed during A's 10 minutes and B's 30 minutes must be
	// consistent with the curve, not inflated by ~100% re-suppression.
	var supA, supB int
	for _, s := range res.Suppressed {
		if !s.Minute.Before(a.From) && s.Minute.Before(a.To) {
			supA += s.Count
		}
		if !s.Minute.Before(b.From) && s.Minute.Before(b.To) {
			supB += s.Count
		}
	}
	if supA == 0 || supB == 0 {
		t.Fatal("expected fresh suppression in both blackout windows")
	}
	// A's recoveries (rf=1.0, within 1m) all land inside B; if they were
	// double-counted as fresh suppression, supB would exceed ~3x its
	// fresh share for these windows (rates ~5-6/min in both).
	if supB > supA*5 {
		t.Fatalf("B's suppression (%d) implausibly inflated vs A's (%d) — re-arrivals double-counted?", supB, supA)
	}
}
