# The portability contract

What a shortfall implementation in another language must satisfy to
interoperate with this one.

Go is the reference implementation. This document is the surface a second
implementation has to match — and the surface an external adapter author
has to code against. It is deliberately *not* a description of the Go
package layout: none of that is portable, and none of it is the contract.

Three audiences, one document:

- someone porting shortfall to another language;
- someone writing an exporter, querier, or middleware for a shortfall
  deployment in a language shortfall does not ship;
- someone reviewing a change to Go that would, quietly, change any of the
  above.

Everything normative here is backed by an ADR and, where it can be, by
machine-checkable conformance vectors. Where a shape is genuinely still
moving, this document says so and says what would settle it. A false
guarantee costs a port more than an honest gap does.

**Keywords.** MUST, MUST NOT, and MAY carry their usual force.

## Contents

- [Conformance vectors](#conformance-vectors)
- [1. Money](#1-money)
- [2. The `biz.vc` wire codec](#2-the-bizvc-wire-codec)
- [3. Context propagation](#3-context-propagation)
- [4. The flow registry](#4-the-flow-registry)
- [5. Metrics and the outcome event](#5-metrics-and-the-outcome-event)
- [6. Behavioural invariants](#6-behavioural-invariants)
- [7. What is still moving](#7-what-is-still-moving)
- [8. Conformance checklist](#8-conformance-checklist)

---

## Conformance vectors

A contract nothing verifies is a wish. Two language-neutral JSON files
carry the checkable half of this document:

| File | Covers |
|---|---|
| [`testkit/vectors/vc-codec.json`](../testkit/vectors/vc-codec.json) | the `biz.vc` wire codec: encodings, accepted non-canonical inputs, rejections |
| [`testkit/vectors/registry.json`](../testkit/vectors/registry.json) | the flow-registry validator: accepted documents and the facts they yield, rejected documents, the propagation allowlist, the duration subset |

They contain no Go and require no Go to consume: load the JSON, feed each
input to your implementation, compare against the expected output. Both
files are produced by running the reference implementation
(`go run ./testkit/cmd/genvectors` from the repo root) and are replayed
back through it by `testkit/vectors_test.go` on every CI run, so the
files, the Go code, and this document cannot drift apart silently. The
same test asserts that every rejection class named below appears in this
file — a class added to the vectors without a line here fails CI.

Two fields need explaining before you use them:

- **`error`** is a stable rejection *class*. It is the portable part: your
  implementation MUST reject the same input, and SHOULD be able to say
  which class it hit. Class names are listed in the tables below.
- **`reference_message`** is the Go implementation's exact diagnostic
  text. It is **not** part of the contract — it is recorded so that a
  wording change shows up as a reviewed diff instead of an invisible one.
  Do not match on it.

**One hazard in the vector files themselves.** `amount_minor` is a JSON
number that reaches the full int64 range. A JSON reader that backs numbers
with a float64 — JavaScript's built-in, and some JVM and Python
configurations — will silently corrupt `9223372036854775807` while
reading the very vector that exists to catch silent money corruption. Use
a bigint-preserving reader for that field.

---

## 1. Money

Governing ADR: [ADR-0001](adr/0001-money-representation.md). Reader-facing
explanation: [money.md](money.md).

### 1.1 The representation

An amount is a triple:

| Part | Type | Meaning |
|---|---|---|
| amount | signed 64-bit integer | the value **in minor units** |
| currency | string | ISO 4217 alphabetic code |
| exponent | signed 8-bit integer | decimal places of the minor unit |

`14900` with currency `USD` and exponent `2` is $149.00. `14900` with
currency `JPY` and exponent `0` is ¥14900. The exponent travels with the
amount because currencies disagree about decimal places, and "cents" is
not a representation.

### 1.2 Floats are disqualified — and so is decimal

No implementation may represent, transport, or accumulate an amount as a
binary floating-point number. That much is the well-known half.

The less obvious half: **`BigDecimal` and `decimal.Decimal` are also not
the answer.** They are correct about arithmetic and wrong about this
contract, for three reasons:

1. The exponent is **declared data**, not a property of a numeric literal.
   `Decimal("149.00")` and `Decimal("149.0")` are equal numbers carrying
   different scales, and neither of them is "USD, exponent 2" — the
   currency's exponent is a fact about the currency, and the amount's
   integer-ness is a fact about the ledger.
2. A decimal type admits division, and division is how a fractional cent
   enters a ledger that has no way to store one.
3. It changes the wire form. The `amount_minor` field is an integer in
   every serialization here; a decimal type will happily render `149.00`
   into it.

Use your language's 64-bit integer: Java `long`, Kotlin `Long`, C# `long`,
Rust `i64`, Python `int` *with the range enforced* (see below). Keep the
exponent as a separate declared field. Statistical code — baselines,
recovery fractions, confidence intervals — uses floats freely; it produces
estimates, which are ranges by construction, and it never touches an
amount.

### 1.3 The int64 range is a wire rule, not an inherited language property

This is the one place the three languages genuinely diverge, and the
divergence is asymmetric:

| Language | Native integer | What that means here |
|---|---|---|
| Go | `int64` | the range **is** the type; overflow is impossible to represent |
| Java | `long` | same range; but `long` arithmetic wraps silently on overflow |
| Python | `int` | arbitrary precision; the range **does not exist** unless you create it |

So the contract states the range itself, rather than pointing at Go:

> An amount MUST be in `[-9223372036854775808, 9223372036854775807]`. An
> implementation MUST reject an amount outside that range **at the
> boundary** — when decoding a wire value, when parsing a registry
> document, when accepting an amount from an application — rather than
> carrying it inward and discovering it at serialization time.

Concretely:

- **Python**: `int` will happily hold `2**63`. Range-check on decode and
  on ingest. The codec vector `amount_past_int64` is exactly this case: an
  implementation that "works" on it has a bug that will surface as a
  corrupt figure in a Finance report, not as an exception.
- **Java**: the range is inherited, but overflow is not an error — `a + b`
  wraps. Summation over a reporting window is the operation this library
  performs most, so use `Math.addExact` (or an equivalent) on every
  accumulation and let it throw.
- **Go**: the range is inherited and overflow also wraps silently; the
  reference implementation's own `ParseMinor` checks before multiplying
  for exactly this reason.

### 1.4 Parsing decimal strings

An implementation that accepts a human decimal string (`"149.00"`) and
converts it to minor units MUST:

- refuse excess precision rather than round — `"149.005"` at exponent 2 is
  a caller bug, and money bugs must be loud;
- refuse a fractional part at all when the exponent is 0;
- refuse a value that would overflow the range in 1.3, *before* the
  multiplication that would wrap;
- never route through a float.

### 1.5 Validity, as distinct from representability

Beyond the range, an amount is *valid* when: it is not negative
(negative amounts are rejected in v0.x — outcomes carry transaction value,
and refunds are a modelling decision for a future ADR, not an accidental
sign); the currency is exactly three uppercase A–Z characters; and the
exponent is in `[0, 4]`.

Validity and representability are **separate judgments**, and section 2.5
explains why the codec deliberately does not conflate them.

### 1.6 Currencies never mix

Sums are per-currency. An implementation MUST NOT add amounts of different
currencies, and MUST NOT ship exchange rates. A normalized column, if a
deployment wants one, is fed by a caller-supplied rate provider and is
presented as a separate figure.

---

## 2. The `biz.vc` wire codec

Governing ADR: [ADR-0003](adr/0003-value-context-propagation.md).

The value context — which flow, which entity, whose money, how much —
crosses a service hop as **one** W3C Baggage member named `biz.vc`. One
member, not eight, because eight members are eight chances to copy seven,
and because an async carrier then copies exactly one header.

A Java service, a Python service and a Go service in one flow MUST produce
byte-identical encodings for the same context. This section is what makes
that true.

### 2.1 Grammar

The member value is eleven `|`-delimited fields, in this order:

```
version | flow | entity_id | customer_id | segment | amount_minor | currency | exponent | kind | flags | deadline_unix
```

The worked example — the `reference` encode vector:

```text
1|invoice.pay|inv_8Ka92j|h:3f9ac2|smb|14900|USD|2|fee|1|1787841120
```

| # | Field | Form |
|---|---|---|
| 0 | `version` | the literal `1` for this codec version |
| 1 | `flow` | escaped string |
| 2 | `entity_id` | escaped string |
| 3 | `customer_id` | escaped string (already hashed by the caller) |
| 4 | `segment` | escaped string, may be empty |
| 5 | `amount_minor` | signed decimal integer, int64 range |
| 6 | `currency` | escaped string |
| 7 | `exponent` | signed decimal integer, **int8** range |
| 8 | `kind` | escaped string |
| 9 | `flags` | `0` or `1` — see 2.4 |
| 10 | `deadline_unix` | unsigned decimal integer, `0` means no deadline |

Fields are positional and never omitted. An empty string field is an empty
field between two delimiters, not an absent one: the vector
`all_optional_fields_empty` (`1|f|e|||0||0||0|0`) is a well-formed
eleven-field encoding in which four of the fields — `customer_id`,
`segment`, `currency`, `kind` — are the empty string.

### 2.2 Escaping

Every string field is escaped **uniformly and byte-wise**:

- A byte MUST be escaped as `%XX` when it is one of `|`, `%`, `"`, `,`,
  `;`, `\`, space (0x20), or when it is below 0x21 or above 0x7E.
- `XX` is **two uppercase hexadecimal digits**. `%7C`, never `%7c`.
- Everything else is emitted literally.

Two properties follow, and both are load-bearing:

- **No field can smuggle a delimiter**, so field-splitting is
  unambiguous.
- **Nothing the W3C Baggage value grammar forbids survives**, so the
  encoded value needs no further quoting at the Baggage layer.

**Escaping is over UTF-8 bytes, not over characters.** This is the trap
for a Java port, whose `String` is UTF-16, and for any implementation that
reaches for a character-oriented loop: `é` is two bytes and becomes
`%C3%A9`, not one escape and not a surrogate pair. The vector
`utf8_escaped_bytewise` pins it. Encode your string to UTF-8 first, then
walk bytes.

Lowercase hex is the single likeliest thing a port gets wrong, because
most URL-decoders accept it. This decoder does not: `lowercase_hex_escape`
is a rejection vector.

### 2.3 Size limit

The encoded member value MUST NOT exceed **512** bytes, measured as the
UTF-8 byte length of the value produced by the rules above — that is,
*after* this codec's own `%XX` escaping and *before* any encoding the
Baggage wire layer adds. (The `biz.vc=` key adds 7 more bytes on the
wire; Baggage's own overall budget is 8KB, shared with other users, and
this library stays a good citizen inside it.)

The limit is enforced **in both directions**: an encoder MUST fail rather
than truncate, and a decoder MUST reject an oversized value *before*
parsing it.

Escaping inflates. 128 percent signs are 384 bytes. An implementation that
checks the pre-escape length has not implemented this rule.

### 2.4 Flags

`flags` carries the `estimated` bit: `1` when the amount came from the
registry estimator rather than from the transaction, `0` otherwise.

Version 1 defines **only** the values `0` and `1`. A decoder MUST reject
any other value, `2` and `3` included. This is deliberate: it means a new
flag cannot be introduced by widening the field, only by bumping the
version token — which is exactly the guarantee that keeps a v1 decoder
from silently mis-reading a v2 producer.

### 2.5 Decoding: fidelity, not validation

A decoder reports **what the wire carried**. It does not apply the
validity rules of 1.5, and an implementation that folds validation into
its decoder will disagree with this one:

| Wire value | Decoder | Validator |
|---|---|---|
| `amount_minor` = `-1` | accepts | rejects (negative) |
| `exponent` = `7` | accepts | rejects (outside `[0, 4]`) |
| `kind` = `mrr` | accepts | rejects (not a declared kind) |

Transport fidelity and semantic validity are separate judgments, made at
separate layers, and the distinction is what lets a receiver distinguish
"the peer sent nonsense" from "the peer sent nothing". Each row above is a
decode vector.

Three outcomes must be **distinguishable** at the extraction seam, and
collapsing them is a defect:

| Outcome | Meaning |
|---|---|
| present, well-formed | a context to use |
| absent | no `biz.vc` member on the carrier |
| present, malformed | a corrupted context — surface it into a counter or log |

A corrupted header MUST NOT be mistaken for an absent one.

### 2.6 Unknown fields, unknown versions

The bead behind this document asked what a decoder does with an unknown
field. The honest answer is that **there is no such thing** in version 1,
and that is the design:

- The field count is exactly 11. Twelve fields is a rejection
  (`too_many_fields`), not eleven fields plus something to ignore.
- The version token is checked first. A decoder MUST reject a version it
  does not know rather than parse optimistically.
- Therefore the encoding evolves by **bumping the version token**, never
  by redefining a field in place or appending one.

A future version-2 producer talking to a version-1 consumer gets a loud
rejection, which is the correct outcome for a wire format that carries
money.

### 2.7 Canonical form, and what a decoder tolerates

There is exactly one canonical encoding of a given context: the one 2.1
and 2.2 produce. A decoder accepts a slightly wider language, because
tolerating these costs nothing and rejecting them would break peers that
are otherwise correct:

| Tolerated | Example | Canonicalizes to |
|---|---|---|
| `%XX` for a byte that needed no escape | `%69nv1` | `inv1` |
| zero-padded integers | `0014900` | `14900` |
| explicitly-signed integers | `+2` | `2` |

Every tolerated form **shrinks or stays the same** on re-encode. That is
not an accident: it means re-encoding a decoded context can never lengthen
it, so the 512-byte budget survives an arbitrary number of hops. An
implementation that adds a tolerance violating this property breaks the
size guarantee for everyone downstream.

Each row is a decode vector carrying both the input and its canonical
form; the vector test asserts the non-growth property directly.

### 2.8 Rejection classes

| Class | Input the contract rejects |
|---|---|
| `oversize` | encoded value longer than 512 bytes, either direction |
| `deadline_domain` | a deadline outside `(1970-01-01, 3000-01-01]` on encode, or outside `[0, 32503680000]` on decode |
| `field_count` | not exactly 11 `\|`-delimited fields |
| `unknown_version` | a leading version token this decoder does not implement |
| `amount_syntax` | `amount_minor` is not a decimal integer |
| `amount_range` | `amount_minor` is a decimal integer outside int64 |
| `exponent_syntax` | `exponent` is not a decimal integer |
| `exponent_range` | `exponent` is a decimal integer outside int8 |
| `escape_syntax` | a malformed `%XX` — bad hex digits, truncated, or lowercase |
| `raw_unescaped_byte` | a literal byte that the escape rule requires be escaped |
| `flags_undefined` | a `flags` value other than `0` or `1` |

`amount_syntax` and `amount_range` are separate classes even though the
reference implementation happens to report them identically: a port MUST
reject both, and the two have different causes worth telling apart.

### 2.9 Deadlines

`deadline_unix` is **unix seconds**, not a formatted timestamp, so that no
port's date library becomes part of the codec contract.

- `0` means "no deadline".
- The encodable domain is therefore *strictly after* the epoch and no
  later than `32503680000` (3000-01-01T00:00:00Z).
- Sub-second precision is discarded at encode; an implementation MUST NOT
  emit fractional seconds.
- Both directions enforce the same bounds, so everything encodable
  decodes, and a peer-controlled header can never smuggle a
  timestamp-overflowing instant into SLA arithmetic.

---

## 3. Context propagation

Governing ADR: [ADR-0003](adr/0003-value-context-propagation.md).

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

A deployment that installs a generic Baggage propagator globally bypasses
this fence. That is a deployment concern, and it must be documented as one
by any implementation that could be deployed that way.

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

## 4. The flow registry

Governing ADRs: [ADR-0003](adr/0003-value-context-propagation.md) (the
propagation allowlist), [ADR-0004](adr/0004-metric-label-set.md) (the
segment enumeration), [ADR-0016](adr/0016-value-stage-anchoring.md) (the
value stage). Field-by-field reference for humans:
[registry.md](registry.md).

Every implementation in a deployment loads the *same* registry file and
MUST validate it *identically*. A file one implementation accepts and
another rejects is a deployment that reports different money depending on
which service was asked.

### 4.1 The smallest valid document

```yaml
version: 1
segments: [default]
flows:
  checkout:
    money: { kind: gmv }
    stages:
      - { name: pay, signals: ["http:POST /pay"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "stripe:charges" }
```

This is the `minimal` acceptance vector, and it is validated on every CI
run by the repository's doc-fence checker. Everything omitted here is
optional; everything present is required.

### 4.2 Validation rules that are easy to get wrong

**Unknown fields are errors.** A key the schema does not define MUST fail
the load. A typoed knob that silently defaults is a fence that is not
there, discovered during an incident.

**A second YAML document is an error.** Content after a `---` separator
MUST fail rather than be ignored, for the same reason.

**Durations are an ISO-8601 *subset*, not ISO-8601.** The accepted grammar
is `P[nD][T[nH][nM][nS]]`, strictly positive, at most ten years, with
units in descending order and days as exact 24-hour days. Months and years
are **rejected**: their length depends on when you ask, and an SLA that
means one thing in February and another in March is not an SLA. A port
that reaches for its standard library's ISO-8601 duration parser will
accept `P1M` and disagree with every other implementation. The `duration`
and `duration_reject` vectors pin the boundary, including the ten-year cap
and the rejection of Go-style `30m`.

**Two fences are declared here and enforced elsewhere.** The `segments`
enumeration is the metric-cardinality fence of 5.2; `propagation.allow_hosts`
is the egress fence of 3.5. Both MUST be validated at load — a malformed
allowlist pattern must fail *here*, not become a near-allow-all at match
time.

**The value stage is derived, not written.** A flow's value stage is its
declared `reconcile.stage` when present, and otherwise its **last**
declared stage. Every implementation must derive it the same way, because
it is where value is counted once per transaction; any cross-stage sum
multiply-counts. The acceptance vectors carry the derived `value_stage`
for exactly this reason.

**Severity thresholds are strictly decreasing.** Floors are minor units
per minute, ordered most-severe first, each strictly less than the one
before, so "the highest severity whose floor this rate clears" is
unambiguous. A duplicate label or an out-of-order floor is a config error,
never a silent tie.

**The estimator's exponent is declared.** It defaults to 2 and MUST be
declarable, so a JPY flow's estimate cannot inherit a USD estimator's
100× error.

### 4.3 Rejection classes

Every class below has at least one vector in
[`registry.json`](../testkit/vectors/registry.json), each differing from
the reference document by exactly one edit — so a vector isolates the one
rule it is named for.

| Class | Rejected because |
|---|---|
| `yaml_syntax` | the document is not well-formed YAML |
| `unknown_field` | a key the schema does not define |
| `multi_document` | content after a `---` separator |
| `version_unsupported` | `version` is not 1 |
| `no_segments` | the segment enumeration is empty or absent |
| `segment_token` | a segment name outside `[a-z0-9._-]`, or longer than 32 |
| `segment_duplicate` | the same segment declared twice |
| `allow_host_pattern` | an allowlist entry that is not a bare lowercase DNS name or `*.domain` |
| `severity_sev_shape` | a severity label that is empty, over 32 characters, or contains whitespace |
| `severity_duplicate` | the same severity label twice |
| `severity_min_per_minute` | a floor that is not positive |
| `severity_order` | a floor not strictly below the previous one |
| `no_flows` | no flows declared |
| `flow_name_token` | a flow name outside `[a-z0-9._-]`, or longer than 64 |
| `money_kind` | `money.kind` outside `gmv`, `net_revenue`, `fee`, `take_rate` |
| `no_stages` | a flow with no stages |
| `stage_token` | a stage name outside `[a-z0-9._-]`, or longer than 32 |
| `stage_duplicate` | the same stage name twice in one flow |
| `stage_no_signals` | a stage declaring no signals |
| `stage_empty_signal` | a signal that is empty or whitespace |
| `currency_code` | a declared currency that is not an ISO 4217 alphabetic code |
| `currency_duplicate` | the same currency twice |
| `sla_unknown_stage` | an SLA naming a stage the flow does not declare |
| `sla_deadline` | a deadline outside the duration subset |
| `sla_on_breach` | `on_breach` other than `lost` or `at_risk` |
| `estimator_default_minor` | a default that is not positive minor units |
| `estimator_segment_unknown` | `by_segment` naming a segment outside the enumeration |
| `estimator_by_segment_value` | a per-segment amount that is not positive |
| `estimator_exponent_range` | an estimator exponent outside `[0, 4]` |
| `baseline_seasonality` | a seasonality model other than `hour_of_week` |
| `baseline_lookback` | `lookback_weeks` below 1 |
| `recovery_model` | a recovery model other than `usage_loss_curve` |
| `recovery_fraction` | `recovered_fraction` outside `[0, 1]` |
| `recovery_within_missing` | a positive recovered fraction with no window |
| `recovery_within_without_fraction` | a window with a zero recovered fraction |
| `reconcile_source_required` | a flow with no reconcile source |
| `reconcile_source_scheme` | a reconcile source outside the known schemes (`sql:`, `stripe:`) |
| `reconcile_stage_unknown` | `reconcile.stage` naming a stage the flow does not declare |

**One known gap in the reference implementation.** The `[0, 1]` bound on
`recovered_fraction` is a pair of ordinary comparisons, and a YAML `.nan`
fails both — so a flow declaring `recovered_fraction: .nan` with no
`within` window loads. (One declaring `.nan` *and* a window is rejected,
but as `recovery_within_without_fraction`, because NaN fails the `> 0`
test too: the right verdict for the wrong reason.) That is a defect in
the reference implementation, tracked separately — not a licence to copy
it. A
conforming implementation MUST reject a non-finite `recovered_fraction`
under `recovery_fraction`. There is no vector for it yet, because a
vector asserts what the reference implementation does, and here that is
the wrong answer; the vector lands with the fix.

### 4.4 Allowlist matching

An entry is either an exact lowercase DNS name or a `*.domain` pattern.

- `*.domain` matches any host one or more labels deeper than `domain`, and
  **never `domain` itself**.
- An exact entry grants nothing below it: `api.example.com` does not
  admit `x.api.example.com`.
- The label boundary is part of the match, so `evil-internal.example.com`
  does not match `*.internal.example.com`.
- The input is a **bare hostname**: no port, no trailing dot. Case is
  normalized; anything else malformed is **denied, not cleaned** —
  cleaning is how `evil.com.` gets past an allowlist.
- An empty or absent allowlist denies everything.

Twelve `host_allowlist` vectors pin these, including both verdicts.

---

## 5. Metrics and the outcome event

Governing ADRs: [ADR-0004](adr/0004-metric-label-set.md) (label sets),
[ADR-0002](adr/0002-outcome-event-transport.md) (event transport and
shape), [ADR-0005](adr/0005-inflight-age-buckets.md) (age buckets),
[ADR-0012](adr/0012-inflight-count-gauge.md) (the in-flight count gauge).
Draft convention write-up: [semconv.md](semconv.md).

### 5.1 What is frozen and what is not

`semconv.md` is marked **draft** as an OpenTelemetry proposal — meaning it
has not been submitted upstream, deliberately, until there is adoption
evidence. That draft status is about *submission*, and a port should not
read it as "these shapes are unsettled". They are not equally settled,
though, so:

| Shape | Status for a port |
|---|---|
| the set of metric families | **frozen** — a new family is an ADR, not a patch |
| each family's label set | **frozen** — a family never gains a label |
| the `outcome` value enumeration | **frozen** |
| the `age_bucket` value enumeration | **frozen** |
| the `reason` value enumeration on drops | **frozen** |
| the label fallbacks (5.3) | **frozen** |
| the *facts* an outcome event carries | **frozen** |
| the *serialized key names* of an outcome event | **NOT settled** — see 7.1 |
| the OTel attribute names in `semconv.md` | draft, pending upstream submission |

### 5.2 Metric families

Six families exist. No implementation may emit a seventh under the `biz_`
prefix, and no family may gain a label.

| Family | Type | Labels |
|---|---|---|
| `biz_value_total` | counter | `flow`, `stage`, `outcome`, `currency`, `kind`, `segment` |
| `biz_txn_total` | counter | `flow`, `stage`, `outcome`, `currency`, `segment` |
| `biz_inflight_value` | gauge | `flow`, `stage`, `age_bucket`, `currency` |
| `biz_inflight_count` | gauge | `flow`, `stage`, `age_bucket`, `currency` |
| `biz_provider_calls_total` | counter | `provider`, `op`, `outcome` |
| `biz_dropped_events_total` | counter | `reason` |

> ADR-0004's own table lists five families and says no family exists
> beyond them; `biz_inflight_count` was added afterwards by ADR-0012, as
> an amendment. Six is the current contract. Reconciling ADR-0004's
> wording with ADR-0012 is tracked separately — see 7.2.

Bounded value enumerations:

- `outcome` ∈ `success`, `failed`, `deferred`, `abandoned`, `unknown`
- `age_bucket` ∈ `lt1m`, `1m-5m`, `5m-30m`, `30m-2h`, `gt2h`
- `reason` ∈ `invalid`, `overflow`, `encode`, `export`

`currency` is the one data-driven label axis, bounded in practice by ISO
4217 and boundable per flow by declaring `currencies` in the registry.
`provider` and `op` are adapter-supplied constants, never request data.

A metric point's value is an **integer**: a counter point is a *delta*
observed at its own timestamp, never a cumulative total; a gauge point is
the level at that timestamp. A batching exporter MUST stamp the backend
with the point's own timestamp, never with flush time — a batch delayed by
an incident must not move money in time.

### 5.3 Label fallbacks

Money is never silently lost to a misconfiguration, and cardinality is
never silently blown:

| Situation | Metric label | Event |
|---|---|---|
| `flow` or `stage` not in the registry | the fixed literal `unregistered` | keeps the raw names, for diagnosis |
| `segment` outside the enumeration | the empty string, with a logged warning | keeps the raw value |

Both fallbacks are part of the contract: sums stay complete, the series
count stays bounded, and the misconfiguration is visible on a dashboard.

### 5.4 Ids never become labels

The entity id and the customer id ride **events only**. Any code path that
would place one on a metric is a defect, and an implementation should make
it structurally impossible rather than discourage it. Per-customer
questions are answered from the event sink — which is precisely why a
metrics-only backend must report the customers leg as unavailable (6.3)
rather than empty.

### 5.5 The outcome event

One event per terminal stage transition, per transaction. It carries the
whole value context plus the transition:

| Fact | Notes |
|---|---|
| event time | the *observation's* time — for a webhook-fed adapter, the provider's event timestamp, not receipt time |
| flow, stage, outcome | outcome from the frozen enumeration |
| entity id | the de-dup key; may repeat across attempts, so it is not unique per transaction |
| customer id | already hashed by the caller; never raw PII |
| segment | may be empty |
| amount (minor units), currency, exponent | as in section 1 |
| kind | the money definition |
| estimated | true when the amount came from the registry estimator |
| deadline | when the context carried one |
| trace id | when a trace exists — never load-bearing |
| source | e.g. `stripe:webhook` |
| error text | short, PII-guarded, bounded at 512 bytes |

The **facts** above are the contract. Their serialized key names are not
yet settled across the repository's own surfaces — see 7.1 before you
pick spellings.

### 5.6 The PII guard

Entity id, customer id, source, and error text pass a PII check before an
event is accepted. Error text is the field that matters most: it is where
an upstream provider's message echoes a card number into every sink the
deployment exports to. An implementation MUST reject an outcome carrying
an email address, a PAN, or an IBAN in those fields, and MUST NOT carry
raw PII in any `biz.*` attribute, fixture, or test datum.

The trace id is guarded by shape rather than by inspection: 32 lowercase
hex characters admit no PII by construction.

---

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

An exporter handed a metric point whose family it does not recognise MUST
surface an error for that batch. It MUST NOT drop the point, and MUST NOT
invent a mapping. A silently unexported family is a metric that reads zero
on a dashboard during the incident it was built for.

### 6.3 An ungrounded leg reports unavailable, never zero

A report leg its backend cannot ground MUST carry a **structural** marker
saying so, and a reason naming why — not a plausible-looking zero, and not
a string convention a renderer has to sniff. "Measured zero" and "never
measured" are different answers to a question Finance is asking, and a
renderer that cannot tell them apart will eventually present the second as
the first. See [ADR-0017](adr/0017-unavailable-leg-marker.md), which
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

## 7. What is still moving

Stated plainly, because a port needs to know where to expect churn more
than it needs a comforting guarantee.

### 7.1 The outcome event's serialized key names

**Status: not settled. Do not treat any single spelling as canonical.**

Three surfaces in this repository spell the event's fields differently:

| Fact | ADR-0002's canonical JSON | `semconv.md` attribute | shipped exporters |
|---|---|---|---|
| flow | `flow` | `biz.flow` | `biz.flow` |
| entity id | `entity_id` | `biz.entity_id` | `biz.entity.id` |
| amount | `amount_minor` | `biz.money.amount_minor` | `biz.amount_minor` |
| kind | `kind` | `biz.kind` | `biz.value.kind` |
| estimated | `est` | `biz.estimated` | `biz.amount.est` |

The **facts** are stable and agreed; only the spellings differ. Until this
is resolved, a port should match the exporter it is writing against — the
shipped exporters' spellings are what existing queries and dashboards
read — and should not assume ADR-0002's JSON literally.

**What would settle it:** one ADR amendment naming one spelling for each
fact, plus a conformance vector file for the event shape (the same
treatment the codec and registry get here) so the three surfaces cannot
drift again. That is a bead, not a paragraph in this document, and it is
deliberately not resolved here: picking a spelling unilaterally would
create a fourth.

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

## 8. Conformance checklist

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

- [ ] the operations in 3.1 carry the in-scope context
- [ ] the context survives every boundary in 3.2
- [ ] the egress fence in 3.5 holds, including the strip-and-rebuild
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

- [ ] exactly the six families of 5.2, with exactly their label sets
- [ ] the frozen value enumerations
- [ ] the label fallbacks of 5.3
- [ ] ids never appear on a metric
- [ ] counter points are deltas stamped with the observation's own time
- [ ] the PII guard covers entity id, customer id, source, and error text

**Behaviour**

- [ ] outcome events emit regardless of trace sampling
- [ ] an unrecognised `biz_*` family errors rather than dropping
- [ ] an ungrounded leg is structurally marked unavailable, never zeroed
- [ ] realized and estimated value are never merged
- [ ] every drop increments `biz_dropped_events_total{reason}`
- [ ] recording never blocks and never fails the business request

---

## Changing this contract

A change to anything specified here is an interface change: it needs an
ADR, and it needs the vectors regenerated in the same pull request
(`go run ./testkit/cmd/genvectors`), with the resulting diff reviewed as
the contract change it is. The vector test replays the committed files on
every CI run and asserts this document names every rejection class they
carry, so a change that touches only one of the three — code, vectors,
contract — fails rather than lands.
