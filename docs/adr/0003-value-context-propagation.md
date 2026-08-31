# ADR-0003 — ValueContext propagates as ONE versioned Baggage member: biz.vc

Status: accepted (ratified at the M2 interface freeze, 2026-08-27)
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
- Size budget: **≤ 512 bytes**, measured as the UTF-8 byte length of the
  encoded member VALUE (the library's own encoding, before any
  percent-encoding the Baggage wire layer adds; the `biz.vc=` key adds 7
  bytes on the wire). Enforced with a typed error — Baggage overall carries
  an 8KB limit shared with other users; we stay a good citizen.
- **Egress scoping (trust boundary):** biz.vc carries a transaction amount
  and a hashed customer id, and the stock OTel Baggage propagator injects
  into *every* outbound request — third-party payment providers and vendor
  APIs included. Therefore: the library's own client RoundTripper injects
  `biz.vc` only toward hosts matching the registry's declared internal
  propagation allowlist (deny by default across origins), and the
  integration guide warns that a globally-installed generic Baggage
  propagator bypasses this and must be scoped by the deployment. Shipping amounts to
  third parties is a decision someone must make on purpose, not a default.
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
