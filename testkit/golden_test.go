package testkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NightWatchEng/shortfall/examples/checkout"
)

// TestGoldensMatchGroundTruth is the drift fence: the committed goldens
// must equal a fresh ground-truth computation of every scenario that
// declares a golden window. CI (ubuntu/amd64) is authoritative: on a
// platform whose math.Exp differs this test may fail locally — adopt the
// values this test prints in CI, never local ones (see the harness
// package doc for the mechanism).
func TestGoldensMatchGroundTruth(t *testing.T) {
	matches, err := filepath.Glob("../examples/checkout/scenarios/*.yaml")
	if err != nil || len(matches) < 3 {
		t.Fatalf("expected the three canonical scenarios, got %v (err %v)", matches, err)
	}
	seen := 0
	for _, path := range matches {
		sc, err := checkout.LoadScenario(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if sc.Golden.From.IsZero() {
			continue
		}
		seen++
		fresh := ComputeExpected(sc.Name, checkout.Run(sc.Config), sc.Golden)
		freshJSON, err := json.MarshalIndent(fresh, "", "  ")
		if err != nil {
			t.Fatal(err)
		}

		committed, err := os.ReadFile(filepath.Join("goldens", sc.Name+".json"))
		if err != nil {
			t.Fatalf("%s: missing golden (run `go run ./testkit/cmd/genexpected` and commit): %v", sc.Name, err)
		}
		// Compare semantically (round-trip both) so formatting never
		// masquerades as drift.
		var a, b Expected
		if err := json.Unmarshal(freshJSON, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(committed, &b); err != nil {
			t.Fatalf("%s: committed golden unparsable: %v", sc.Name, err)
		}
		if a != b {
			t.Errorf("%s: golden drift.\nfresh (adopt this if this run is CI):\n%s\ncommitted:\n%s",
				sc.Name, freshJSON, committed)
		}
	}
	if seen < 3 {
		t.Fatalf("only %d scenarios declare golden windows; the three canonical ones must", seen)
	}
}

// TestGoldensAreNonTrivial guards against a silently degenerate harness:
// each canonical scenario's golden must actually exercise its locus.
func TestGoldensAreNonTrivial(t *testing.T) {
	read := func(name string) Expected {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("goldens", name+".json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var e Expected
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return e
	}

	api := read("api-5xx")
	if api.Realized.Count == 0 || api.Realized.ValueMinor == 0 {
		t.Error("api-5xx golden has no realized loss")
	}
	if api.Unrealized.AbandonedCount == 0 {
		t.Error("api-5xx golden has no abandonment")
	}
	if api.Customers.Distinct == 0 {
		t.Error("api-5xx golden impacted no customers")
	}

	stall := read("queue-stall")
	if stall.Deferred.Count == 0 || stall.Deferred.ValueMinor == 0 {
		t.Error("queue-stall golden has no deferred value at the snapshot")
	}
	if stall.Deferred.CaptureBreaches == 0 {
		t.Error("queue-stall golden breaches no capture SLA despite a 45m stall vs 30m SLA")
	}
	if stall.Realized.Count != 0 {
		t.Error("queue-stall golden shows realized loss — a stall defers, it does not fail")
	}

	blackout := read("upstream-blackout")
	if blackout.Unrealized.SuppressedCount == 0 {
		t.Error("upstream-blackout golden suppressed nothing")
	}
	if blackout.Unrealized.RecoveredCount == 0 {
		t.Error("upstream-blackout golden recovered nothing despite recovered_fraction 0.6")
	}
	if blackout.Unrealized.NetLostCount <= 0 {
		t.Error("upstream-blackout golden lost nothing net")
	}
	if blackout.Unrealized.NetLostValueMinorEst == 0 {
		t.Error("upstream-blackout golden net-lost value estimate is zero")
	}
}
