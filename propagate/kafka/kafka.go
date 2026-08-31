// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package kafka carries ValueContext across Kafka message headers without
// importing any Kafka client library. Kafka headers are ordered key/[]byte
// pairs; this package mirrors that shape with a local Header type, so
// callers convert their client's headers to/from []Header with a trivial
// loop and no dependency crosses the boundary.
package kafka

import "github.com/NightWatchEng/shortfall/propagate"

// Header mirrors a Kafka record header (key + raw bytes). kafka-go,
// segmentio, and confluent-kafka-go use {Key string, Value []byte}
// exactly (a field copy); sarama's RecordHeader.Key is []byte, so a
// sarama caller does string(h.Key) on the way in. No client library is
// imported either way.
type Header struct {
	Key   string
	Value []byte
}

// Carrier adapts a header slice to propagate.Carrier. Set replaces an
// existing header with the same key (Kafka permits duplicates; a single
// canonical biz.vc is what the consumer must read), matching last-write
// semantics. The slice is addressed through a pointer so Set can grow it.
type Carrier struct{ Headers *[]Header }

var _ propagate.Carrier = Carrier{}

// NewCarrier wraps a header slice pointer.
func NewCarrier(headers *[]Header) Carrier { return Carrier{Headers: headers} }

// Get returns the value of the first header with key, as a string.
func (c Carrier) Get(key string) string {
	if c.Headers == nil {
		return ""
	}

	for _, h := range *c.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}

	return ""
}

// Set makes key canonical: the first matching header takes the value and
// every later duplicate (Kafka permits them) is removed, so a consumer
// reads one unambiguous value regardless of its client's dup semantics.
// Returns false on a nil backing pointer so Inject fails loudly.
func (c Carrier) Set(key, value string) bool {
	if c.Headers == nil {
		return false
	}

	set := false
	out := (*c.Headers)[:0]
	for _, h := range *c.Headers {
		if h.Key == key {
			if set {
				continue // drop later duplicate
			}

			h.Value = []byte(value)
			set = true
		}

		out = append(out, h)
	}

	if !set {
		out = append(out, Header{Key: key, Value: []byte(value)})
	}

	*c.Headers = out
	return true
}

// Keys lists the header keys in order.
func (c Carrier) Keys() []string {
	if c.Headers == nil {
		return nil
	}

	out := make([]string, 0, len(*c.Headers))
	for _, h := range *c.Headers {
		out = append(out, h.Key)
	}

	return out
}
