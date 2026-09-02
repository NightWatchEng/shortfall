// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHardcodedFormatVocabulary enforces what main.go's usage() comment
// claims: every statement of the format vocabulary reads formatVocabulary.
// The needle is built FROM the vocabulary rather than written out, so this
// test carries no copy of the list to go stale — and so it keeps working if
// the list ever changes.
//
// This is the guard for the drift the shared dispatcher exists to prevent:
// two of these enumerations were left hardcoded across two review rounds, and
// nothing went red either time.
func TestNoHardcodedFormatVocabulary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	needle := formatUsage()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		checked++
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Clean(name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}

			if strings.Contains(string(b), needle) {
				t.Errorf("%s hardcodes %q — interpolate formatUsage() instead, "+
					"or a new format name will leave this string advertising the old set", name, needle)
			}
		})
	}

	// Without this the test passes vacuously if the walk ever matches nothing.
	if checked == 0 {
		t.Fatal("no non-test .go files were scanned — the guard checked nothing")
	}
}
