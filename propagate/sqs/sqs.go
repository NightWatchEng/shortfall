// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package sqs carries ValueContext across SQS message attributes without
// importing the AWS SDK. SQS string attributes are {DataType:"String",
// StringValue:...}; this package mirrors that with a local Attribute type,
// so callers convert to/from their SDK's MessageAttributeValue map with a
// short loop (AWS SDK v2 uses *string fields: aws.String on write,
// aws.ToString on read) and no dependency crosses the boundary.
package sqs

import "github.com/NightWatchEng/shortfall/propagate"

// Attribute mirrors an SQS message attribute's string form. DataType is
// set to "String" on write, the value the SDK requires.
type Attribute struct {
	DataType    string
	StringValue string
}

// Carrier adapts an attribute map to propagate.Carrier.
type Carrier struct{ Attrs map[string]Attribute }

var _ propagate.Carrier = Carrier{}

// NewCarrier wraps an attribute map (nil-safe on read; allocate before
// Inject).
func NewCarrier(attrs map[string]Attribute) Carrier { return Carrier{Attrs: attrs} }

// Get returns the string value of the attribute with key.
func (c Carrier) Get(key string) string { return c.Attrs[key].StringValue }

// Set writes a String attribute and returns true; on a nil map it writes
// nothing and returns false so Inject fails loudly (allocate the map
// before injecting).
func (c Carrier) Set(key, value string) bool {
	if c.Attrs == nil {
		return false
	}
	c.Attrs[key] = Attribute{DataType: "String", StringValue: value}
	return true
}

// Keys lists the attribute keys.
func (c Carrier) Keys() []string {
	out := make([]string, 0, len(c.Attrs))
	for k := range c.Attrs {
		out = append(out, k)
	}
	return out
}
