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

// atKey is the one field a file of events carries that a log store would
// own instead: the event time, RFC3339. A line without it is validated at
// check time — time is not what this verb checks — but a line that has it
// must have it well-formed, because a store that later reads it will not
// guess either.
const atKey = "at"

// knownKeys is every top-level key an outcome line may carry: the biz.*
// contract (biz/semconv.go), the diagnostic fields, the event marker, and
// the file-only at field. Anything else in the biz.* namespace is an
// addition the contract never agreed to — the exact drift the exporters'
// contract test catches on the Go side, applied here to a line some other
// language wrote.
var knownKeys = map[string]bool{
	biz.EventKey: true, atKey: true,
	biz.AttrFlow: true, biz.AttrStage: true, biz.AttrOutcome: true,
	biz.AttrEntityID: true, biz.AttrCustomerID: true, biz.AttrSegment: true,
	biz.AttrAmountMinor: true, biz.AttrCurrency: true, biz.AttrExponent: true,
	biz.AttrValueKind: true, biz.AttrAmountEst: true, biz.AttrSLADeadline: true,
	biz.AttrSource: true, biz.AttrError: true, biz.AttrTraceID: true,
	// source_system is the spelling since-removed exporters wrote; the
	// decoder still accepts it, so the checker does too.
	"source_system": true,
}

// runCheckEvents implements `shortfall check-events <file.jsonl>`: every
// non-blank line is decoded as one outcome event on the wire contract and
// validated at the biz boundary — the same fences emit.Record applies —
// so a service written in another language can prove its events would be
// accepted before any land in a store. Exit 0 when every line passes, 1
// when any is rejected (each named by line and defect), 2 on usage.
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
	if rejected > 0 {
		return 1
	}

	return 0
}

// checkEventLine holds one line to the contract: unknown biz.* keys are
// rejected, at (if present) must parse, and the decoded outcome must pass
// biz.Outcome.Validate.
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

	at := time.Now().UTC()
	if rawAt, present := fields[atKey]; present {
		var s string
		if err := json.Unmarshal(rawAt, &s); err != nil {
			return fmt.Errorf("%s must be an RFC3339 string: %w", atKey, err)
		}

		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("%s: %w", atKey, err)
		}

		at = parsed
	}

	out, err := eventline.Parse(raw, at)
	if err != nil {
		return err
	}

	return out.Validate()
}
