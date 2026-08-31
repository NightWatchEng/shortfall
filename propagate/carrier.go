// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package propagate carries ValueContext across message queues as one
// header — the biz.vc member (ADR-0003) — so async consumers re-attach
// the same context. It defines the Carrier seam (Get/Set/Keys over
// string), and the kafka, sqs, and amqp subpackages provide Carriers over
// each transport's header shape without importing that transport's client
// library: a Prometheus user never pulls a Kafka SDK into their build.
package propagate

import (
	"fmt"

	"github.com/NightWatchEng/shortfall/biz"
)

// Carrier is the minimal read/write surface a message's headers must
// present. It follows the OpenTelemetry propagator shape (Get/Set/Keys),
// with one deliberate change: Set returns whether the write landed, so a
// nil backing store fails loudly instead of losing context silently.
type Carrier interface {
	Get(key string) string
	// Set writes the value under key and reports whether the write
	// landed. A carrier over a nil/unwritable backing store returns
	// false so Inject can fail loudly instead of losing money context
	// on the hop while the caller believes it propagated.
	Set(key, value string) bool
	Keys() []string
}

// Inject encodes vc and writes it under the single biz.vc key. It fails
// (without writing) when the context is invalid or exceeds the codec's
// 512-byte cap — one header, size-bounded, versionable.
func Inject(c Carrier, vc biz.ValueContext) error {
	enc, err := biz.EncodeVC(vc)
	if err != nil {
		return err
	}
	if !c.Set(biz.MemberKey, enc) {
		return fmt.Errorf(
			"propagate: carrier could not hold biz.vc (nil or unwritable backing store) — context not propagated",
		)
	}
	return nil
}

// Extract reads the biz.vc header if present. It returns (vc, true, nil)
// when a well-formed member is found, (_, false, nil) when absent, and
// (_, false, err) when present-but-corrupt — a mangled header is never
// mistaken for an absent one.
func Extract(c Carrier) (biz.ValueContext, bool, error) {
	raw := c.Get(biz.MemberKey)
	if raw == "" {
		return biz.ValueContext{}, false, nil
	}
	vc, err := biz.DecodeVC(raw)
	if err != nil {
		return biz.ValueContext{}, false, err
	}
	return vc, true, nil
}
