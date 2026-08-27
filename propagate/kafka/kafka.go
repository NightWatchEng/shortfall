// Package kafka carries ValueContext across Kafka message headers without
// importing any Kafka client library. Kafka headers are ordered key/[]byte
// pairs; this package mirrors that shape with a local Header type, so
// callers convert their client's headers to/from []Header with a trivial
// loop and no dependency crosses the boundary.
package kafka

import "github.com/NightWatchEng/shortfall/propagate"

// Header mirrors a Kafka record header (key + raw bytes). It intentionally
// matches the field shape every Kafka client uses, so conversion is a
// field copy.
type Header struct {
	Key   string
	Value []byte
}

// Carrier adapts a header slice to propagate.Carrier. Set REPLACES an
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

// Set replaces the header with key, or appends it.
func (c Carrier) Set(key, value string) {
	if c.Headers == nil {
		return
	}
	for i := range *c.Headers {
		if (*c.Headers)[i].Key == key {
			(*c.Headers)[i].Value = []byte(value)
			return
		}
	}
	*c.Headers = append(*c.Headers, Header{Key: key, Value: []byte(value)})
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
