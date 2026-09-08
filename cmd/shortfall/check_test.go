// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEvents writes a JSON-lines file and returns its path.
func writeEvents(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

const okEvent = `{"event":"biz.outcome","biz.flow":"invoice.pay","biz.stage":"capture","biz.outcome":"failed",` +
	`"biz.entity.id":"inv_000042","biz.customer.id":"h:c0ffee","biz.segment":"smb",` +
	`"biz.amount.minor":14900,"biz.amount.currency":"USD","biz.amount.exponent":2,` +
	`"biz.value.kind":"fee","biz.amount.estimated":false,"source":"billing-svc","error":"card_declined"}`

func TestCheckEventsAcceptsConformantFiles(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{"two events and a blank line", []string{okEvent, "", okEvent}},
		// `at` stands in for the store's timestamp when the events come
		// from a file; a well-formed one is accepted alongside the rest.
		{"with an at field", []string{strings.Replace(okEvent, `"source"`, `"at":"2026-08-28T14:05:00Z","source"`, 1), okEvent}},
		// `time` is what the Cloud Logging exporter writes; a producer can
		// check the exact bytes it ships.
		{"with a time field", []string{strings.Replace(okEvent, `"source"`, `"time":"2026-08-28T14:05:00.123456789Z","source"`, 1), okEvent}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runCheckEvents([]string{writeEvents(t, c.lines...)}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, stderr.String())
			}

			if !strings.Contains(stdout.String(), "2 event(s) ok, 0 rejected") {
				t.Fatalf("summary missing: %q", stdout.String())
			}
		})
	}
}

func TestCheckEventsNamesEachRejectionByLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // substring of the diagnostic
	}{
		{"not json", `{nope`, "line 1:"},
		{"foreign line", `{"level":"info","msg":"started"}`, "event must be the string"},
		{"no event marker", strings.Replace(okEvent, `"event":"biz.outcome",`, ``, 1), "event must be the string"},
		{"wrong event marker", strings.Replace(okEvent, `"biz.outcome",`, `"something.else",`, 1), "event must be the string"},
		{"fractional money", strings.Replace(okEvent, `"biz.amount.minor":14900`, `"biz.amount.minor":149.00`, 1), "not an int64"},
		{"quoted money", strings.Replace(okEvent, `"biz.amount.minor":14900`, `"biz.amount.minor":"14900"`, 1), "must be a JSON number"},
		{"quoted exponent", strings.Replace(okEvent, `"biz.amount.exponent":2`, `"biz.amount.exponent":"2"`, 1), "must be a JSON number"},
		{"quoted boolean", strings.Replace(okEvent, `"biz.amount.estimated":false`, `"biz.amount.estimated":"false"`, 1), "biz.amount.estimated"},
		{"empty segment", strings.Replace(okEvent, `"biz.segment":"smb"`, `"biz.segment":""`, 1), "requires it absent"},
		{"empty error", strings.Replace(okEvent, `"error":"card_declined"`, `"error":""`, 1), "requires it absent"},
		{"null segment", strings.Replace(okEvent, `"biz.segment":"smb"`, `"biz.segment":null`, 1), "is null"},
		{"null estimated", strings.Replace(okEvent, `"biz.amount.estimated":false`, `"biz.amount.estimated":null`, 1), "is null"},
		{"null time", strings.Replace(okEvent, `"source"`, `"time":null,"source"`, 1), "is null"},
		{"email in the id", strings.Replace(okEvent, `"h:c0ffee"`, `"jo@example.com"`, 1), "email"},
		{"undeclared kind", strings.Replace(okEvent, `"fee"`, `"revenue"`, 1), "kind"},
		{"bad exponent", strings.Replace(okEvent, `"biz.amount.exponent":2`, `"biz.amount.exponent":7`, 1), "exponent"},
		{"unknown biz attribute", strings.Replace(okEvent, `"biz.segment":"smb"`, `"biz.segment":"smb","biz.region":"eu"`, 1), "biz.region"},
		{"missing exponent", strings.Replace(okEvent, `"biz.amount.exponent":2,`, ``, 1), "biz.amount.exponent is missing"},
		{"missing estimated", strings.Replace(okEvent, `"biz.amount.estimated":false,`, ``, 1), "biz.amount.estimated is missing"},
		{"missing flow", strings.Replace(okEvent, `"biz.flow":"invoice.pay",`, ``, 1), "biz.flow is missing"},
		{"malformed at", strings.Replace(okEvent, `"source"`, `"at":"yesterday","source"`, 1), "at:"},
		{"malformed time", strings.Replace(okEvent, `"source"`, `"time":"yesterday","source"`, 1), "time:"},
		{"non-string at", strings.Replace(okEvent, `"source"`, `"at":1756389900,"source"`, 1), "at must be an RFC3339 string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCheckEvents([]string{writeEvents(t, c.line)}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
			}

			if !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("diagnostic %q does not name the defect (%q)", stderr.String(), c.want)
			}

			if !strings.Contains(stdout.String(), "0 event(s) ok, 1 rejected") {
				t.Fatalf("summary missing: %q", stdout.String())
			}
		})
	}
}

func TestCheckEventsRefusesAFileWithNoEvents(t *testing.T) {
	// A gate that passes on nothing is not a gate: an empty fixture is the
	// #55 vacuous shape, and it fails loudly rather than reading as green.
	cases := []struct {
		name  string
		lines []string
	}{
		{"zero bytes", nil},
		{"blank lines only", []string{"", "   ", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runCheckEvents([]string{writeEvents(t, c.lines...)}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}

			if !strings.Contains(stderr.String(), "holds no events") {
				t.Fatalf("stderr %q must say nothing was checked", stderr.String())
			}
		})
	}
}
