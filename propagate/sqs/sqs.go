// Package sqs carries ValueContext across SQS message attributes without
// importing the AWS SDK. SQS string attributes are {DataType:"String",
// StringValue:...}; this package mirrors that with a local Attribute type,
// so callers convert to/from their SDK's MessageAttributeValue map with a
// trivial loop and no dependency crosses the boundary.
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

// Set writes a String attribute. It is a no-op on a nil map — allocate
// the map before injecting.
func (c Carrier) Set(key, value string) {
	if c.Attrs == nil {
		return
	}
	c.Attrs[key] = Attribute{DataType: "String", StringValue: value}
}

// Keys lists the attribute keys.
func (c Carrier) Keys() []string {
	out := make([]string, 0, len(c.Attrs))
	for k := range c.Attrs {
		out = append(out, k)
	}
	return out
}
