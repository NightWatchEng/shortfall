// Package propagate carries ValueContext across message queues as ONE
// header — the biz.vc member (ADR-0003) — so async consumers re-attach
// the same context. It defines the Carrier seam (Get/Set/Keys over
// string), and the kafka, sqs, and amqp subpackages provide Carriers over
// each transport's header shape WITHOUT importing that transport's client
// library: a Prometheus user never pulls a Kafka SDK into their build.
package propagate

import "github.com/NightWatchEng/shortfall/biz"

// Carrier is the minimal read/write surface a message's headers must
// present. It mirrors the shape OpenTelemetry propagators use, so an
// existing carrier adapter works here unchanged.
type Carrier interface {
	Get(key string) string
	Set(key, value string)
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
	c.Set(biz.MemberKey, enc)
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
