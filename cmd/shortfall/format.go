// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"slices"
	"strings"
)

// formatVocabulary is the single list of accepted --format values. The flag
// help, the early rejection and the dispatcher all read it, so no verb can
// accept a name another one rejects — impact and reconcile render the two
// halves of one report and a reader pasting both into a postmortem needs the
// same format to work for each.
var formatVocabulary = []string{"text", "json", "markdown"}

// formatUsage renders the vocabulary for a usage or error string.
func formatUsage() string { return strings.Join(formatVocabulary, "|") }

// knownFormat reports whether name is one this CLI renders. Both verbs check
// it before they build a querier, so a typo fails immediately instead of after
// a full backend round-trip.
func knownFormat(name string) bool { return slices.Contains(formatVocabulary, name) }

// renderings is the three ways one report kind can be written out.
type renderings struct {
	text     func() string
	markdown func() string
	json     func() ([]byte, error)
}

// renderFormat writes r in the named format and returns the process exit code.
// The default arm stays a real guard rather than an assertion: a caller that
// skips knownFormat must still be refused, not silently given text.
func renderFormat(stdout, stderr io.Writer, format string, r renderings) int {
	switch format {
	case "text":
		ws(stdout, r.text())
	case "markdown":
		ws(stdout, r.markdown())
	case "json":
		b, err := r.json()
		if err != nil {
			wf(stderr, "render: %v\n", err)
			return 1
		}

		wln(stdout, string(b))
	default:
		wf(stderr, "--format: unknown %q (want %s)\n", format, formatUsage())
		return 2
	}

	return 0
}
