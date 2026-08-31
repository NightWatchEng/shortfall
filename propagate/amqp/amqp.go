// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package amqp carries ValueContext across AMQP header tables without
// importing an AMQP client library. AMQP headers are a Table
// (map[string]interface{}); this package adapts that shape directly, so a
// caller passes its client's header table with no conversion and no
// dependency crosses the boundary.
package amqp

import "github.com/NightWatchEng/shortfall/propagate"

// Carrier adapts an AMQP header table (map[string]interface{}) to
// propagate.Carrier. Reads accept string or []byte values (clients vary);
// writes store strings.
type Carrier struct{ Table map[string]interface{} }

var _ propagate.Carrier = Carrier{}

// NewCarrier wraps a header table (nil-safe on read; allocate before
// Inject).
func NewCarrier(table map[string]interface{}) Carrier { return Carrier{Table: table} }

// Get returns the value of key as a string, accepting string or []byte.
func (c Carrier) Get(key string) string {
	switch v := c.Table[key].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// Set stores value as a string and returns true; on a nil table it
// writes nothing and returns false so Inject fails loudly (allocate the
// table before injecting).
func (c Carrier) Set(key, value string) bool {
	if c.Table == nil {
		return false
	}
	c.Table[key] = value
	return true
}

// Keys lists the table keys.
func (c Carrier) Keys() []string {
	out := make([]string, 0, len(c.Table))
	for k := range c.Table {
		out = append(out, k)
	}
	return out
}
