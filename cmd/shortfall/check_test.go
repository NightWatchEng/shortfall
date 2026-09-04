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

func TestCheckEventsAcceptsAConformantFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCheckEvents([]string{writeEvents(t, okEvent, "", okEvent)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "2 event(s) ok, 0 rejected") {
		t.Fatalf("summary missing: %q", stdout.String())
	}
}

func TestCheckEventsNamesEachRejectionByLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // substring of the diagnostic
	}{
		{"not json", `{nope`, "line 1:"},
		{"foreign line", `{"level":"info","msg":"started"}`, "not a biz outcome line"},
		{"fractional money", strings.Replace(okEvent, `"biz.amount.minor":14900`, `"biz.amount.minor":149.00`, 1), "not an int64"},
		{"email in the id", strings.Replace(okEvent, `"h:c0ffee"`, `"jo@example.com"`, 1), "email"},
		{"undeclared kind", strings.Replace(okEvent, `"fee"`, `"revenue"`, 1), "kind"},
		{"bad exponent", strings.Replace(okEvent, `"biz.amount.exponent":2`, `"biz.amount.exponent":7`, 1), "exponent"},
		{"unknown biz attribute", strings.Replace(okEvent, `"biz.segment":"smb"`, `"biz.segment":"smb","biz.region":"eu"`, 1), "biz.region"},
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

func TestCheckEventsHonoursAnAtField(t *testing.T) {
	// `at` is the store's timestamp when the line comes from a file rather
	// than a log store; a malformed one is a rejection, not a silent now().
	var stdout, stderr bytes.Buffer
	bad := strings.Replace(okEvent, `"source"`, `"at":"yesterday","source"`, 1)
	if code := runCheckEvents([]string{writeEvents(t, bad)}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "at") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	good := strings.Replace(okEvent, `"source"`, `"at":"2026-08-28T14:05:00Z","source"`, 1)
	if code := runCheckEvents([]string{writeEvents(t, good)}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestCheckEventsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCheckEvents(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("no file: exit %d, want 2", code)
	}

	if code := runCheckEvents([]string{filepath.Join(t.TempDir(), "missing.jsonl")}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing file: exit %d, want 1", code)
	}
}
