## 2. The `biz.vc` wire codec

Governing ADR: [ADR-0003](../adr/0003-value-context-propagation.md).

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
