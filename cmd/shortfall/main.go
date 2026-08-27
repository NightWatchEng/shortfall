// Command shortfall is the CLI: validate a flow registry, compute an
// impact report for an incident window, reconcile telemetry against a
// ledger, and simulate scenarios. Subcommands arrive with their engine
// milestones; the arg surface stays hand-rolled until there is more than
// one real verb to route.
package main

import (
	"fmt"
	"os"

	"github.com/NightWatchEng/shortfall/registry"
)

// Stamped by goreleaser via -ldflags -X (see .goreleaser.yaml); "dev"/"none"
// identify a non-release build.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "--version", "version":
		fmt.Printf("shortfall %s (%s)\n", version, commit)
		return 0
	case "validate":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: shortfall validate <registry.yaml>")
			return 2
		}
		reg, err := registry.Load(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("%s: ok — %d flow(s), %d segment(s)\n", args[1], len(reg.FlowNames()), len(reg.Segments))
		return 0
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `shortfall — incident $ impact
usage:
  shortfall validate <registry.yaml>   validate a flow registry
  shortfall version                    print build provenance`)
}
