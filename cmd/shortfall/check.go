// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/eventline"
)

// timeKeys are the fields that carry the event time on a line: "time" is
// what the Cloud Logging exporter writes and the agent adopts as the
// entry's timestamp, and "at" is the file-only stand-in this verb also
// accepts. A line without either is validated at check time — time is not
// what this verb checks — but one that has it must have it well-formed
// (RFC3339), because a store that later reads it will not guess either.
var timeKeys = []string{"time", "at"}

// knownKeys is every top-level key an outcome line may carry: the biz.*
// contract (biz/semconv.go), the diagnostic fields, the event marker, and
// the file-only at field. Anything else in the biz.* namespace is an
// addition the contract never agreed to — the exact drift the exporters'
// contract test catches on the Go side, applied here to a line some other
// language wrote.
var knownKeys = map[string]bool{
	biz.EventKey: true, "time": true, "at": true,
	biz.AttrFlow: true, biz.AttrStage: true, biz.AttrOutcome: true,
	biz.AttrEntityID: true, biz.AttrCustomerID: true, biz.AttrSegment: true,
	biz.AttrAmountMinor: true, biz.AttrCurrency: true, biz.AttrExponent: true,
	biz.AttrValueKind: true, biz.AttrAmountEst: true, biz.AttrSLADeadline: true,
	biz.AttrSource: true, biz.AttrError: true, biz.AttrTraceID: true,
	// source_system is the spelling since-removed exporters wrote; the
	// decoder still accepts it, so the checker does too.
	"source_system": true,
}

// requiredKeys must be present on every line: the attribute set the
// contract vector's required_only case carries. The decoder fills an
// absent one with its zero value — exponent 0, estimated false — which
// reads as a different, valid fact, so presence has to be checked here
// rather than left to validation.
var requiredKeys = []string{
	biz.AttrFlow, biz.AttrStage, biz.AttrOutcome, biz.AttrEntityID, biz.AttrCustomerID,
	biz.AttrAmountMinor, biz.AttrCurrency, biz.AttrExponent, biz.AttrValueKind, biz.AttrAmountEst,
}

// numericKeys must be JSON numbers on the wire. The decoder happens to
// tolerate a quoted numeral, so this check is stricter than it: a producer
// that writes "14900" has a type bug the contract vector would fail it on,
// and this verb exists to say so before a store does.
var numericKeys = []string{biz.AttrAmountMinor, biz.AttrExponent}

// absentNotEmpty are the optional keys the contract requires to be absent
// rather than empty (testkit/vectors/outcome-event.json, required_only).
// An empty string decodes to the same zero value absence does, so only a
// check at the raw-JSON layer can tell them apart.
var absentNotEmpty = []string{biz.AttrSegment, biz.AttrSLADeadline, biz.AttrSource, biz.AttrError, biz.AttrTraceID}

// runCheckEvents implements `shortfall check-events <file.jsonl>`: every
// non-blank line is decoded as one outcome event on the wire contract and
// validated at the biz boundary — the same fences emit.Record applies —
// so a service written in another language can prove its events would be
// accepted before any land in a store. It is stricter than the decoders
// where the contract is: the event marker and every required attribute
// must be present, numbers must be numbers, and optional facts must be
// absent rather than empty. Exit 0 when every line passes, 1 when any is
// rejected (each named by line and defect) or when the file holds no
// events at all — a gate that passes on nothing is not a gate — and 2 on
// usage.
func runCheckEvents(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		wln(stderr, "usage: shortfall check-events <events.jsonl>")
		return 2
	}

	f, err := os.Open(args[0])
	if err != nil {
		wf(stderr, "check-events: %v\n", err)
		return 1
	}

	defer func() { _ = f.Close() }()

	ok, rejected := 0, 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}

		if err := checkEventLine([]byte(raw)); err != nil {
			wf(stderr, "line %d: %v\n", n, err)
			rejected++
			continue
		}

		ok++
	}

	if err := sc.Err(); err != nil {
		wf(stderr, "check-events: %v\n", err)
		return 1
	}

	wf(stdout, "%s: %d event(s) ok, %d rejected\n", args[0], ok, rejected)
	if ok+rejected == 0 {
		wf(stderr, "check-events: %s holds no events — nothing was checked\n", args[0])
		return 1
	}

	if rejected > 0 {
		return 1
	}

	return 0
}

// checkEventLine holds one line to the contract: the event marker must
// name this library's record, unknown biz.* keys are rejected, every
// required attribute is present, numbers are JSON numbers, optional facts
// are absent rather than empty, the time field (if present) parses, and
// the decoded outcome passes biz.Outcome.Validate.
func checkEventLine(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}

	for k := range fields {
		if strings.HasPrefix(k, "biz.") && !knownKeys[k] {
			return fmt.Errorf("%s is not in the outcome-event contract (biz/semconv.go)", k)
		}
	}

	// The log-store queriers select on this marker; a line without it is
	// never read back, however well-formed the rest is.
	var marker string
	if raw, ok := fields[biz.EventKey]; !ok || json.Unmarshal(raw, &marker) != nil || marker != biz.EventOutcome {
		return fmt.Errorf("%s must be the string %q — the log-store queriers select on it", biz.EventKey, biz.EventOutcome)
	}

	for _, k := range requiredKeys {
		if _, ok := fields[k]; !ok {
			return fmt.Errorf("%s is missing — the contract requires it on every event", k)
		}
	}

	for _, k := range numericKeys {
		if raw, ok := fields[k]; ok && !isNumberToken(raw) {
			return fmt.Errorf("%s must be a JSON number, not %s", k, raw)
		}
	}

	for _, k := range absentNotEmpty {
		if raw, ok := fields[k]; ok && string(raw) == `""` {
			return fmt.Errorf("%s is present as an empty string — the contract requires it absent", k)
		}
	}

	at := time.Now().UTC()
	for _, k := range timeKeys {
		rawAt, present := fields[k]
		if !present {
			continue
		}

		var s string
		if err := json.Unmarshal(rawAt, &s); err != nil {
			return fmt.Errorf("%s must be an RFC3339 string: %w", k, err)
		}

		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}

		at = parsed
	}

	out, err := eventline.Parse(raw, at)
	if err != nil {
		return err
	}

	return out.Validate()
}

// isNumberToken reports whether a raw JSON value is a number token rather
// than a string or anything else — a leading digit or minus sign.
func isNumberToken(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && (t[0] == '-' || (t[0] >= '0' && t[0] <= '9'))
}
