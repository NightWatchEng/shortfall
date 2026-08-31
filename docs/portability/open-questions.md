## 7. What is still moving

Stated plainly, because a port needs to know where to expect churn more
than it needs a comforting guarantee.

### 7.1 The outcome event's serialized key names — **settled 2026-08-30**

**Status: settled.** The names are those in 5.5, defined once in code as the
`biz.Attr*` constants and pinned by
[`testkit/vectors/outcome-event.json`](../../testkit/vectors/outcome-event.json).
A port matches that file.

This section previously read *"not settled — do not treat any single
spelling as canonical"* and tabulated three disagreeing columns: ADR-0002's
canonical JSON (`flow`, `entity_id`, `amount_minor`, `kind`, `est`),
`semconv.md` (`biz.entity_id`, `biz.money.amount_minor`, `biz.kind`,
`biz.estimated`), and the shipped exporters (`biz.entity.id`,
`biz.amount_minor`, `biz.value.kind`, `biz.amount.est`). The facts were
never in dispute; only the spellings were.

**How it was settled.** The exporters won, because all three already agreed
with each other — the disagreement was entirely between the code and its own
documentation, so adopting their spelling moved no bytes on any wire. ADR-0002
and `semconv.md` were corrected to match, both carrying dated amendment notes
recording what they had said. The names then stopped being literals: they are
`biz.Attr*` constants that every exporter and querier references, so a rename
is a compile error rather than a silent divergence.

**One later rename (2026-08-30, workspace-8yq).** After the settlement, the
four money attributes moved into a consistent dotted namespace —
`biz.amount.minor`, `biz.amount.currency`, `biz.amount.exponent`,
`biz.amount.estimated` — a founder-approved pre-1.0 wire-format break. The
settlement's mechanism is what made it cheap: the vector and the constants
changed together, and every exporter followed by compilation.

**What now holds it.** `testkit.CheckOutcomeEvent` drives every event-capable
exporter through the vector, and `TestEveryExporterChecksTheEventContract`
fails the build of an exporter that does not run it. The vector records which
names must be **absent** rather than empty, because an optional field nobody
checked is how `biz.sla.deadline` came to ship on OTLP alone.

**One declared difference remains**, and it is a difference in carriage, not
in the field set: OTLP puts the trace id on the log record's span context,
which is what a trace-aware transport should do. Every other transport must
emit `trace.id` as an attribute, and the vector requires it of them.

### 7.2 ADR-0004's family count

ADR-0004 states that no family exists beyond the five in its table;
ADR-0012 then added `biz_inflight_count`. The contract is six (5.2), but
the ADR text has not been amended to say so. **What would settle it:** an
amendment note in ADR-0004 pointing at ADR-0012, in a PR whose tracked
item is about the ADR text.

### 7.3 Negative amounts

Rejected in v0.x. Refunds, chargebacks, and adjustments are a real
modelling question that a future ADR will answer, and the wire format
already carries the sign (2.5). **What would settle it:** an ADR deciding
whether a refund is a negative amount, a distinct kind, or a distinct
outcome result. Until then, a port should reject negatives at validation
and *not* invent semantics.

### 7.4 Semantic-convention submission

`semconv.md` is an unsubmitted proposal, on purpose, pending internal
adoption evidence. Its attribute names may change in response to upstream
review. A port should not tie a public interface to them yet.

### 7.5 The `unavailable:` string prefix

Human-facing prose convention only. The **boolean/structural marker** is
the contract (6.3); the prefix is not, and a port must not parse it.

---
