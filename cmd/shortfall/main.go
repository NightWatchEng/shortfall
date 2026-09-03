// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Command shortfall is the CLI: validate a flow registry, compute an
// impact report for an incident window, reconcile telemetry against a
// ledger. The arg surface stays hand-rolled
// until the verb set outgrows it.
package main

import (
	"fmt"
	"os"

	"github.com/NightWatchEng/shortfall/registry"
)

// Stamped by goreleaser via -ldflags -X (see .goreleaser.yaml). Left at the
// dev sentinels, resolveBuild falls back to the toolchain's build info, so an
// installed binary still names its version.
var (
	version = devVersion
	commit  = devCommit
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
		fmt.Println(versionLine())
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
	case "impact":
		return runImpact(args[1:], os.Stdout, os.Stderr)
	case "reconcile":
		return runReconcile(args[1:], os.Stdout, os.Stderr)
	default:
		usage()
		return 2
	}
}

func usage() {
	// The format list interpolates rather than repeating it. Every statement
	// of the vocabulary in this package reads formatVocabulary;
	// TestNoHardcodedFormatVocabulary is what keeps that true.
	fmt.Fprintf(os.Stderr, `shortfall — incident $ impact
usage:
  shortfall validate <registry.yaml>   validate a flow registry
  shortfall impact --registry r.yaml --from <RFC3339> --to <RFC3339> [--flow f]...
                   [--scope k=v]... [--prometheus URL] [--sql DSN] [--format %[1]s]
                                       compute the impact ledger for a window
  shortfall reconcile --registry r.yaml --from <RFC3339> --to <RFC3339> --ledger rows.json [--flow f]...
                   [--prometheus URL] [--sql DSN] [--source label] [--format %[1]s]
                                       publish the coverage ratio (telemetry vs a ledger)
  shortfall version                    print build provenance
`, formatUsage())
}
