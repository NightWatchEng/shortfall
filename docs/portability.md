# The portability contract

What a shortfall implementation in another language must satisfy to
interoperate with this one.

Go is the reference implementation. This document is the surface a second
implementation has to match — and the surface an external adapter author
has to code against. It is deliberately *not* a description of the Go
package layout: none of that is portable, and none of it is the contract.

Three audiences:

- someone porting shortfall to another language;
- someone writing an exporter, querier, or middleware for a shortfall
  deployment in a language shortfall does not ship;
- someone reviewing a change to Go that would, quietly, change any of the
  above.

Everything normative here is backed by an ADR and, where it can be, by
machine-checkable conformance vectors. Where a shape is still moving, this
document says so, and says what would settle it.

**Keywords.** MUST, MUST NOT, and MAY carry their usual force.

**Porting?** Start at part 8, the checklist — it is the working list, and
each item points back at the part that defines it.

## The contract, in eight parts

| | | |
|---|---|---|
| 1 | [Money](portability/money.md) | int64 minor units, the exponent, why decimal is also wrong |
| 2 | [The `biz.vc` wire codec](portability/wire-codec.md) | the eleven-field grammar, escaping, the 512-byte cap |
| 3 | [Context propagation](portability/propagation.md) | what must carry it, what must not, the egress fence |
| 4 | [The flow registry](portability/registry.md) | the validator contract and its rejection classes |
| 5 | [Metrics and the outcome event](portability/telemetry.md) | the six families, their labels, the event's field set |
| 6 | [Behavioural invariants](portability/invariants.md) | what an implementation must do, not just emit |
| 7 | [What is still moving](portability/open-questions.md) | the shapes not yet frozen, and what would settle them |
| 8 | [Conformance checklist](portability/checklist.md) | the list to work through when porting |

## Conformance vectors

Three language-neutral JSON files carry the checkable half of this
document:

| File | Covers |
|---|---|
| [`testkit/vectors/vc-codec.json`](../testkit/vectors/vc-codec.json) | the `biz.vc` wire codec: encodings, accepted non-canonical inputs, rejections |
| [`testkit/vectors/registry.json`](../testkit/vectors/registry.json) | the flow-registry validator: accepted documents and the facts they yield, rejected documents, the propagation allowlist, the duration subset |
| [`testkit/vectors/outcome-event.json`](../testkit/vectors/outcome-event.json) | the outcome event's serialized field set: which names carry which facts, and which must be **absent** rather than empty (5.5, 7.1) |

They contain no Go and require no Go to consume: load the JSON, feed each
input to your implementation, compare against the expected output. All three
are produced by running the reference implementation
(`go run ./testkit/cmd/genvectors` from the repo root) and are replayed back
through it on every CI run — the first two by `testkit/vectors_test.go`, the
third by each event-capable exporter's own contract test, which
`TestEveryExporterChecksTheEventContract` requires it to have. So the files,
the Go code, and this document cannot drift apart silently. The
same test asserts that every rejection class appears somewhere in this
contract — index or chapter — so a class added to the vectors without a
line documenting it fails CI.

Two fields need explaining before you use them:

- **`error`** is a stable rejection *class*. It is the portable part: your
  implementation MUST reject the same input, and SHOULD be able to say
  which class it hit. Class names are listed in the rejection tables of
  parts 2 and 4.
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

## Changing this contract

A change to anything specified here is an interface change: it needs an
ADR, and it needs the vectors regenerated in the same pull request
(`go run ./testkit/cmd/genvectors`), with the resulting diff reviewed as
the contract change it is. The vector test replays the committed files on
every CI run and asserts this document names every rejection class they
carry, so a change that touches only one of the three — code, vectors,
contract — fails rather than lands.
