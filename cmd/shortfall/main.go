// Command shortfall is the CLI: validate a flow registry, compute an
// impact report for an incident window, reconcile telemetry against a
// ledger, and simulate scenarios.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "shortfall: no subcommands implemented yet (pre-release skeleton)")
	os.Exit(2)
}
