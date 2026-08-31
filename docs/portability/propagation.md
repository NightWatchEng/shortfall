## 3. Context propagation

Governing ADR: [ADR-0003](../adr/0003-value-context-propagation.md).

**This contract cannot specify a mechanism, and does not try.** Go threads
an explicit `context.Context`. Java has `ThreadLocal`, Scoped Values, and
whatever the application framework already does. Python has `contextvars`,
with its own rules across `await` and thread pools. There is no shape
these share, so specifying one would either exclude two of the three
languages or describe none of them honestly.

What is specifiable is the **observable requirement**: which operations
carry the value context, and across which boundaries it survives.

### 3.1 Operations that MUST carry the value context

| Operation | Requirement |
|---|---|
| recording a stage transition | uses the value context in scope for the unit of work; it is not re-supplied by the call site |
| an outbound HTTP request to an allowlisted host | injects the in-scope context as `biz.vc` |
| publishing to a queue | injects the in-scope context as the single `biz.vc` header/property |
| emitting an outcome event | carries the full context, ids included |
| emitting a metric point | carries only the bounded labels of the context — never the ids |

"In scope for the unit of work" is the whole of the requirement. Whether
that scope is an argument, a thread-local, a scoped value, or a context
variable is an implementation's business.

### 3.2 Boundaries the context MUST survive

- ordinary function calls within the unit of work;
- an async continuation belonging to the same logical request — a
  goroutine, a `CompletableFuture` stage, an `await`, a task submitted to
  an executor on behalf of this request;
- an outbound HTTP hop to an allowlisted host, where the receiver
  re-establishes it from the `baggage` header;
- a queue hop, where the consumer re-establishes it from the copied
  header.

### 3.3 Boundaries the context MUST NOT cross

- an outbound request to a host outside the registry's propagation
  allowlist. See 3.5 — this is a trust boundary, not a performance
  optimization.
- a hop with no carrier at all. Losing context there is expected; losing
  it *silently* is not (3.4).

### 3.4 Loss must be observable

A propagation failure MUST be visible. Concretely:

- a carrier whose backing store cannot be written reports the failure to
  the injector, which fails loudly, rather than returning success while
  the context is dropped;
- a present-but-corrupt inbound context is logged or counted distinctly
  from an absent one (2.5);
- an ingress stamp that fails validation is rejected loudly — and the
  request itself still proceeds. Instrumentation never fails a business
  request.

### 3.5 The egress fence

The stock Baggage propagator injects into *every* outbound request,
third-party payment providers and vendor APIs included. `biz.vc` carries a
transaction amount and a customer handle. Shipping those to a third party
is a decision someone must make on purpose.

An implementation's own outbound client therefore MUST:

- inject `biz.vc` only toward hosts matching the registry's declared
  propagation allowlist (deny by default, including when the allowlist is
  empty or absent);
- **remove** `biz.vc` from the outbound `baggage` header toward any other
  host — including a member some globally-installed propagator added, and
  including one hidden behind a malformed neighbouring member;
- rebuild the outbound header from the members it parsed rather than
  forwarding the original bytes, so a malformed or multi-line header
  cannot smuggle a member past the fence;
- pass foreign (non-`biz.vc`) members through in every case;
- re-evaluate per redirect hop, so a redirect from an allowed host to a
  disallowed one is fenced at the second hop;
- fail closed: if a safe header cannot be expressed, send no `baggage`
  header rather than forward a possibly-leaky original.

The fence is the client transport, so exactly two things escape it, and an
implementation MUST document both as deployment concerns:

- a request that never reaches the fence — issued through a client the
  implementation does not own, an SDK holding its own, or any path the
  deployment did not route through the fenced transport; and
- a baggage injector composed *inside* the fenced transport rather than
  around it, which re-injects after the removal above. Composition order
  is part of the contract, not an implementation detail.

Both are available in every deployment, so neither is conditional. A
globally installed generic propagator is the usual source of the injected
member in both cases, and is not itself the hazard: through the fence its
member is removed like any other.

Allowlist matching is specified in 4.4 and covered by the
`host_allowlist` vectors.

### 3.6 Language hazards worth naming

Not normative — but each of these has cost someone a week.

- **Python.** `contextvars` values set inside a task do not escape to the
  parent, so stamping must happen *before* the task is created.
  `ThreadPoolExecutor` does not carry context variables into worker
  threads unless you run the work through `contextvars.copy_context()`.
- **Java.** A `ThreadLocal` is not inherited by a pooled thread, and
  `InheritableThreadLocal` behaves worse than it looks under pooling.
  Scoped Values bind for a dynamic extent, which fits this requirement
  well. If an OpenTelemetry agent is already managing context, attach to
  its mechanism instead of adding a second one — two context stores that
  can disagree are worse than either alone.
- **Go.** A goroutine outliving its request keeps the context object but
  not the request's cancellation semantics; that is fine for value
  context and wrong for anything else riding the same object.

---
