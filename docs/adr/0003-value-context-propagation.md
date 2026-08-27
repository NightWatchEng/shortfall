# ADR-0003 — ValueContext propagates as ONE versioned Baggage member: biz.vc

Status: proposed (ratify at the M2 interface freeze)
Date: 2026-08-27

## Context

The recurring in-house failure is "correlation_id sometimes isn't there":
value context is dropped at exactly the hop that fails. W3C Baggage already
propagates over HTTP in any OTel-instrumented service. Queue transports
(Kafka, SQS, AMQP) carry headers but drop trace context by convention;
whatever we propagate must be trivially copyable into a message header by a
thin wrapper.

Eight separate Baggage members would mean eight chances to copy seven.

## Decision

- The whole ValueContext (flow, entity id, hashed customer id, segment,
  amount, currency, kind, estimated, deadline) is encoded into a **single
  Baggage member named `biz.vc`**, compact and **versioned** (a leading
  version token, so decoders can evolve without breaking old producers).
- Size budget: **≤ 512 bytes encoded**, enforced with a typed error —
  Baggage overall carries an 8KB limit shared with other users; we stay a
  good citizen.
- Queue carriers expose `Get/Set/Keys` Carrier interfaces and copy exactly
  one header. No queue client libraries are imported by `propagate/*`.
- The codec is fuzz-tested (round-trip, 1M iterations) and benchmarked; it
  runs on every request in adopting services.

## Consequences

- Async consumers re-attach identical context with one header copy; a
  failing consumer already carries flow, entity, and amount.
- The encoding is a public wire contract: changes require a version bump in
  the token, never an in-place redefinition.
- Anything beyond 512 bytes (huge segment names, absurd entity ids) fails
  loudly at encode time instead of being silently truncated downstream.
