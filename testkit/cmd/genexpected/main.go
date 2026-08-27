// Command genexpected regenerates the committed golden expected-value
// fixtures from the canonical scenarios. Run from the repo root:
//
//	go run ./testkit/cmd/genexpected
//
// AUTHORITY NOTE: goldens are CI-authoritative (ubuntu/amd64). If your
// platform's math.Exp differs, TestGoldensMatchGroundTruth fails in CI
// and prints the CI-computed values — adopt those, never your local ones.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Nigthwatch-eng/shortfall/examples/checkout"
	"github.com/Nigthwatch-eng/shortfall/testkit"
)

func main() {
	matches, err := filepath.Glob("examples/checkout/scenarios/*.yaml")
	if err != nil || len(matches) == 0 {
		fmt.Fprintln(os.Stderr, "genexpected: no scenarios found — run from the repo root")
		os.Exit(2)
	}
	for _, path := range matches {
		sc, err := checkout.LoadScenario(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genexpected: %v\n", err)
			os.Exit(2)
		}
		if sc.Golden.From.IsZero() {
			fmt.Fprintf(os.Stderr, "genexpected: %s declares no golden window — skipping\n", sc.Name)
			continue
		}
		exp := testkit.ComputeExpected(sc.Name, checkout.Run(sc.Config), sc.Golden)
		b, err := json.MarshalIndent(exp, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "genexpected: %v\n", err)
			os.Exit(2)
		}
		out := filepath.Join("testkit", "goldens", sc.Name+".json")
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "genexpected: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("wrote %s\n", out)
	}
}
