// Command shortfall is the CLI: validate a flow registry, compute an
// impact report for an incident window, reconcile telemetry against a
// ledger, and simulate scenarios.
package main

import (
	"fmt"
	"os"
)

// Stamped by goreleaser via -ldflags -X (see .goreleaser.yaml); "dev"/"none"
// identify a non-release build.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("shortfall %s (%s)\n", version, commit)
		return
	}
	fmt.Fprintln(os.Stderr, "shortfall: no subcommands implemented yet (pre-release skeleton)")
	os.Exit(2)
}
