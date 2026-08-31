# 8. Conformance checklist

A claim of parity means all of these, in order. Anything less is an
assertion.

**Money**

- [ ] amounts are 64-bit integers in minor units with a separately
      declared exponent; no float, no decimal type, anywhere
- [ ] the int64 range is *enforced* at every boundary, not inherited
- [ ] accumulation cannot wrap silently
- [ ] decimal-string parsing refuses excess precision rather than rounding
- [ ] amounts of different currencies are never summed

**The `biz.vc` codec**

- [ ] every `encode` vector produces the committed bytes exactly
- [ ] every `decode` vector yields the committed context and re-encodes
      to the committed canonical form
- [ ] re-encoding never lengthens the value
- [ ] every `encode_reject` and `decode_reject` vector is rejected
- [ ] escaping is byte-wise over UTF-8, with uppercase hex
- [ ] the 512-byte cap is enforced on encode *and* decode
- [ ] present-well-formed, absent, and present-corrupt are three
      distinguishable outcomes

**Propagation**

- [ ] the operations in [3.1](propagation.md#31-operations-that-must-carry-the-value-context) carry the in-scope context
- [ ] the context survives every boundary in [3.2](propagation.md#32-boundaries-the-context-must-survive)
- [ ] the egress fence in [3.5](propagation.md#35-the-egress-fence) holds, including the strip-and-rebuild
      behaviour and the per-redirect re-evaluation
- [ ] every `host_allowlist` vector matches
- [ ] a failed injection is loud

**Registry**

- [ ] every `accept` vector loads and yields the committed facts,
      derived `value_stage` included
- [ ] every `reject` vector fails to load
- [ ] every `duration` and `duration_reject` vector matches
- [ ] unknown fields and second documents are errors

**Metrics and events**

- [ ] exactly the six families of [5.2](telemetry.md#52-metric-families), with exactly their label sets
- [ ] the frozen value enumerations
- [ ] the label fallbacks of [5.3](telemetry.md#53-label-fallbacks)
- [ ] ids never appear on a metric
- [ ] counter points are deltas stamped with the observation's own time
- [ ] the PII guard covers entity id, customer id, source, and error text

**Behaviour**

- [ ] outcome events emit regardless of trace sampling
- [ ] a metrics-capable exporter errors on an unrecognised `biz_*` family rather than dropping or guessing its kind; an exporter declaring `Metrics: false` no-ops on metric points instead of erroring
- [ ] an ungrounded leg is structurally marked unavailable, never zeroed
- [ ] realized and estimated value are never merged
- [ ] every drop increments `biz_dropped_events_total{reason}`
- [ ] recording never blocks and never fails the business request

---
