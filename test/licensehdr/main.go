// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Command licensehdr is the deterministic slice of the attribution
// convention: every tracked .go file carries the two-line Apache-2.0
// header, so a file that travels out of this repo still names its
// author. Run `go run ./test/licensehdr` from the repo root to list
// files missing the header, and `go run ./test/licensehdr -fix` to
// insert it. The test in this module runs the same check, which binds
// the convention into the ordinary test step with no gate wiring.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// header is the exact text inserted by -fix; headerRE is what the check
// accepts, deliberately looser only in the year so a future sweep can
// extend the range without rewriting every file.
const header = "// Copyright 2026 Yauvan Suba\n// SPDX-License-Identifier: Apache-2.0\n"

var headerRE = regexp.MustCompile(`\A// Copyright 20\d\d(-20\d\d)? Yauvan Suba\n// SPDX-License-Identifier: Apache-2\.0\n`)

// generatedRE is the convention from the Go source tree: generated
// files declare themselves and are exempt from the header.
var generatedRE = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// trackedGoFiles asks git for the .go files under root so untracked
// scratch files cannot fail the check.
func trackedGoFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, filepath.Join(root, string(f)))
		}
	}

	return files, nil
}

// needsHeader reports whether the file content is subject to and
// missing the header.
func needsHeader(content []byte) bool {
	if generatedRE.Match(content) {
		return false
	}

	return !headerRE.Match(content)
}

// withHeader returns content with the header prepended, separated by a
// blank line so a leading package doc comment stays attached to its
// package clause and a leading //go:build constraint stays legal.
func withHeader(content []byte) []byte {
	return append([]byte(header+"\n"), content...)
}

// run checks every tracked .go file under root, fixing in place when
// fix is set, and returns the files that were missing the header.
func run(root string, fix bool) ([]string, error) {
	files, err := trackedGoFiles(root)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}

		if !needsHeader(content) {
			continue
		}

		missing = append(missing, f)
		if fix {
			if err := os.WriteFile(f, withHeader(content), 0o644); err != nil {
				return nil, err
			}
		}
	}

	return missing, nil
}

func main() {
	fix := flag.Bool("fix", false, "insert the header into files missing it")
	root := flag.String("root", ".", "repository root to scan")
	flag.Parse()
	missing, err := run(*root, *fix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensehdr:", err)
		os.Exit(2)
	}

	if *fix {
		fmt.Printf("licensehdr: added the header to %d file(s)\n", len(missing))
		return
	}

	if len(missing) > 0 {
		for _, f := range missing {
			fmt.Println(f)
		}

		fmt.Fprintf(os.Stderr, "licensehdr: %d file(s) missing the Apache-2.0 header (run: go run ./test/licensehdr -fix)\n", len(missing))
		os.Exit(1)
	}
}
