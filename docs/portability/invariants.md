## 6. Behavioural invariants

These are not data shapes, and a port that reproduces every byte above
while breaking one of these has not ported shortfall. They are the
library's reason for existing.

### 6.1 Outcome events emit regardless of trace sampling

Money accounting never depends on a sampler. Any code path that gates an
outcome event on a sampling decision is a defect, not a tuning knob. The
per-transaction event is the deterministic half of the library — realized
loss, customer impact, reconciliation all read it — and a sampled-away
event is a dollar figure that is quietly wrong.

### 6.2 An unrecognised `biz_*` family fails loudly

An exporter that declares `Metrics: true` and is handed a metric point whose
family it does not recognise MUST surface an error for that batch. It MUST
NOT drop the point, and MUST NOT invent a mapping. A silently unexported
family is a metric that reads zero on a dashboard during the incident it was
built for, and a family guessed into the wrong kind is worse: an unrecognised
*level* shipped as a monotonic counter is summed by the backend, which is
silently wrong arithmetic on money rather than a loud stop.

The qualifier is load-bearing, not a hedge. An exporter declaring
`Metrics: false` still receives metric points — the emitting layer hands
every batch to whatever exporter it holds, without consulting the declared
capability first — and it MUST no-op on them rather than error. Erroring
would be read as a failed batch, and the emitting layer answers a failed
metric batch by re-crediting its drop counters and warning; an events-only
exporter would then warn on every flush, for a signal it never claimed to
carry. Recognising no families at all is not the same as failing to
recognise one.

### 6.3 An ungrounded leg reports unavailable, never zero

A report leg its backend cannot ground MUST carry a **structural** marker
saying so, and a reason naming why — not a plausible-looking zero, and not
a string convention a renderer has to sniff. "Measured zero" and "never
measured" are different answers to a question Finance is asking, and a
renderer that cannot tell them apart will eventually present the second as
the first. See [ADR-0017](../adr/0017-unavailable-leg-marker.md), which
exists because a `Summary()` line did exactly that.

The commonest instance: a metrics-only backend cannot answer per-customer
questions (5.4), so the customers leg is unavailable — not empty.

### 6.4 Realized and estimated value are never merged

An estimated amount rides the outcome event with its `estimated` flag and
is **excluded** from `biz_value_total`; the transaction is still counted
in `biz_txn_total`. No renderer, exporter, or consumer may add a realized
figure to an estimated one and present a single number. Uncertainty is
expressed by the flag and by ranges at the report layer, never by mixing.

### 6.5 Drops are counted, never silent

Recording an outcome MUST NOT block the business request path and MUST NOT
propagate an error to it. In exchange, every discarded event is counted on
`biz_dropped_events_total{reason}` with the reason from the frozen
enumeration. A visible drop is a coverage-ratio conversation; a silent one
is a number that lies. An export failure that loses events without
incrementing a visible counter is a defect.

The counters themselves are the record of the damage: an implementation
that fails to export a batch containing drop counters must preserve them
rather than lose the evidence of its own outage.

### 6.6 De-duplication distinguishes a retry from a transition

The in-process de-dup key includes the *result*, so a retry of the same
(flow, entity, stage, result) is suppressed while a `failed → success`
transition always emits. Suppressing the transition would corrupt the
realized leg. An overflow drops the *whole* observation — event, metric
increments, and de-dup memory together — so a retry after the buffer
drains emits cleanly rather than double-counting.

### 6.7 Instrumentation never fails the request

An invalid ingress stamp, an unencodable context, an oversized value, a
backend that is down: each is logged, counted where a counter exists, and
otherwise ignored. The business request proceeds. A library that measures
revenue must not be able to cost any.

---
