## 1. Money

Governing ADR: [ADR-0001](../adr/0001-money-representation.md). Reader-facing
explanation: [money.md](../money.md).

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
