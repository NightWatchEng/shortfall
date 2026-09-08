# Emit from any language

shortfall ships a Go emitter, but the report does not care who wrote the
telemetry. The engine reads two signals from a backend — outcome
**events** and `biz_*` **metrics** — and both have a fixed, language-neutral
wire shape. A service in Java, Python, Node or anything else that sits in
a flow can write those shapes itself, land them in the same store its Go
neighbours use, and appear in the same four-leg report.

This page is the short path: what to write, where to put it, and how to
prove it is right before an incident depends on it. The full normative
contract a complete port must satisfy is the
[portability contract](portability.md); you do not need all of it to emit
from one service.

## What you emit

One **outcome event** per stage transition, per transaction. That single
signal grounds realized loss (de-duplicated by entity) and customer
impact — the two legs Finance reconciles first.

Metrics are optional for a single service and needed for the other two
legs: the deferred leg reads the `biz_inflight_*` gauges and the
unrealized leg reads `biz_txn_total` history. The six families and their
label sets are in [telemetry §5.2](portability/telemetry.md); if your Go
neighbours already ship them for the flow's entry stage, you may not need
to.

## The outcome event

One JSON object per event. This is exactly what the shipped exporters
write, so a line you produce is indistinguishable from one the Go emitter
produced:

```json
{
  "event": "biz.outcome",
  "time": "2026-08-28T14:05:00Z",
  "biz.flow": "invoice.pay",
  "biz.stage": "capture",
  "biz.outcome": "failed",
  "biz.entity.id": "inv_000042",
  "biz.customer.id": "h:c0ffee",
  "biz.segment": "smb",
  "biz.amount.minor": 14900,
  "biz.amount.currency": "USD",
  "biz.amount.exponent": 2,
  "biz.value.kind": "fee",
  "biz.amount.estimated": false,
  "source": "billing-svc",
  "error": "card_declined"
}
```

| Key | Type | Rule |
|---|---|---|
| `event` | string | the literal `biz.outcome`; marks the record as this library's in a shared sink |
| `time` | string | RFC 3339 event time — the store adopts it as the entry's timestamp; see below for the other stores' spelling |
| `biz.flow` | string | a flow the registry declares; lowercase `[a-z0-9._-]`, ≤ 64 |
| `biz.stage` | string | a stage of that flow; same charset, ≤ 32 |
| `biz.outcome` | string | one of `success`, `failed`, `deferred`, `abandoned`, `unknown` |
| `biz.entity.id` | string | the idempotency key retries share — realized loss de-duplicates on it; printable ASCII, ≤ 128 |
| `biz.customer.id` | string | **already hashed** by you; may be empty; printable ASCII, ≤ 128 |
| `biz.segment` | string | a registry segment, or **absent** |
| `biz.amount.minor` | **integer** | minor units (`14900` = $149.00 at exponent 2); never a float, never negative |
| `biz.amount.currency` | string | ISO 4217 alphabetic code, uppercase |
| `biz.amount.exponent` | integer | the currency's decimal places, 0–4 |
| `biz.value.kind` | string | one of `gmv`, `net_revenue`, `fee`, `take_rate`, as the registry declares for the flow |
| `biz.amount.estimated` | **boolean** | `true` only when the amount came from the registry estimator |
| `biz.sla.deadline` | string | RFC 3339, only when the context carries a deadline; otherwise absent |
| `source` | string | where the outcome was observed, e.g. `billing-svc`; optional |
| `error` | string | short failure text, ≤ 512 bytes; optional |
| `trace.id` | string | 32 lowercase hex, when a trace exists; optional |

Three things a JSON library will get wrong on your behalf:

- **Types are the contract.** `"14900"` is not `14900`, and `"false"` is
  not `false`. A lenient reader may decode the quoted numeral; the checker
  below rejects it, because the contract vector a port is held to would.
- **Absent, not empty.** An optional fact you do not have is a key you do
  not write. `"biz.segment": ""` is a claim that the segment is the empty
  string, and the checker below rejects it.
- **No PII.** Entity id, customer id, source and error text are checked
  for email addresses, card numbers (Luhn-valid 13–19 digit runs) and
  IBANs, and rejected. Hash the account id before it reaches the event;
  there is deliberately no hashing helper to import.

The event time is the store's timestamp, and you set it from the line:
the Cloud Logging exporter writes a `time` key, which the logging agent
adopts as the entry's timestamp; the CloudWatch exporter writes it as the
EMF `_aws.Timestamp` (epoch milliseconds) alongside the same `biz.*`
keys; the SQL table has an `at` column. Whichever store you use, write the
*event's* time there, not the moment you observed it: a provider webhook
replayed hours late must land in the incident it belongs to, and a store
that stamps receipt time would move that money into the wrong window.

## Where to land it

Pick the store a query adapter already reads; the engine does the rest.

**A log store, one JSON line per event.** Write the object above as a
single line to stdout or your log pipeline. On AWS the line lands in
CloudWatch Logs and `adapters/query/cwinsights` reads it back; on Google
Cloud it lands in Cloud Logging as a `jsonPayload` and
`adapters/query/gcplogging` reads it. Both queriers accept a line from
any writer — they parse the shape, not the author.

**The SQL table.** The CLI's `--sql` flag and `adapters/query/sql` read
one fixed table; insert a row per event. Column names differ from the
JSON keys, and `at` is the event time in Unix **nanoseconds**:

```sql
INSERT INTO biz_outcomes
  (flow, stage, outcome, currency, segment, kind, customer_id, entity_id, amount_minor, at)
VALUES
  ('invoice.pay', 'capture', 'failed', 'USD', 'smb', 'fee', 'h:c0ffee', 'inv_000042', 14900, 1756389900000000000);
```

**An OTLP log record.** Emit a log record named `biz.outcome` whose
attributes are the keys above (the `event` key becomes the record's event
name; `trace.id` rides the span context instead of an attribute). Your
collector routes it wherever it routes the Go emitter's records.

## Check it before it counts

Write a handful of events to a file, one object per line, and hold them
to the contract with the CLI. It applies the same fences the Go emitter
applies at `Record` — charset, bounds, types, PII, the declared
enumerations — and names every rejection by line:

```sh
shortfall check-events events.jsonl
```

```text
line 3: biz: customer id contains an email address — PII must never enter biz.* attributes
line 7: eventline: amount_minor "149.00" is not an int64: strconv.ParseInt: parsing "149.00": invalid syntax
events.jsonl: 5 event(s) ok, 2 rejected
```

Exit status is 0 when every line passes and 1 otherwise, so the check
runs in CI against a fixture your service's test suite writes. It is
stricter than the stores' own readers where the contract is: the `event`
marker must be present, because the log-store queriers select on it and a
line without it is never read back; numbers must be JSON numbers; and the
optional keys must be absent rather than empty. A file that holds no
events fails too, so an empty fixture cannot read as green. The `time`
key (or a file-only `at`) is parsed as RFC 3339 when present; nothing
else outside the table above is allowed under the `biz.` prefix.

A minimal producer, in Python, to show how little there is:

```python
import json, sys
from datetime import datetime, timezone

def outcome(at, flow, stage, result, entity_id, customer_hash, amount_minor, currency, exponent, kind, **opt):
    ev = {
        "event": "biz.outcome",
        "time": at.strftime("%Y-%m-%dT%H:%M:%SZ"),   # the event's own time, not now()
        "biz.flow": flow, "biz.stage": stage, "biz.outcome": result,
        "biz.entity.id": entity_id, "biz.customer.id": customer_hash,
        "biz.amount.minor": int(amount_minor),      # integer minor units, never float
        "biz.amount.currency": currency, "biz.amount.exponent": int(exponent),
        "biz.value.kind": kind, "biz.amount.estimated": False,
    }
    for k in ("segment", "source", "error"):          # absent, not empty
        if opt.get(k):
            ev["biz.segment" if k == "segment" else k] = opt[k]
    sys.stdout.write(json.dumps(ev, separators=(",", ":")) + "\n")

outcome(datetime.now(timezone.utc), "invoice.pay", "capture", "failed", "inv_000042", "h:c0ffee",
        14900, "USD", 2, "fee", segment="smb", source="billing-svc", error="card_declined")
```

## Carrying the context across a hop

When your service calls, or is called by, another service in the same
flow, the value context crosses as **one** W3C Baggage member named
`biz.vc`, so the next hop records against the same entity and dollars:

```text
baggage: biz.vc=1|invoice.pay|inv_000042|h:c0ffee|smb|14900|USD|2|fee|0|0
```

Eleven `|`-separated fields, positional, with a byte-wise `%XX` escape
for the delimiter set — the grammar, the escaping rules and the 512-byte
cap are in [wire codec §2](portability/wire-codec.md), and
`testkit/vectors/vc-codec.json` holds encode, decode and rejection cases
you can replay without Go. Inject it only toward hosts inside your own
estate: the Go transport strips it toward anything else, and a service
that forwards it to a payment provider has shipped transaction amounts
to a third party.

If you only consume the header — a worker that receives a message the Go
producer stamped — decode the member, and write your outcome event from
its fields. That is the whole integration for a mid-flow service.

## Going further

The three vector files under `testkit/vectors/` pin the codec, the
registry validator and the event field set; a full port works through the
[conformance checklist](portability/checklist.md). A metrics-emitting
service follows [telemetry §5.2](portability/telemetry.md) exactly: six
families, fixed labels, integer values, counter deltas stamped with their
own observation time.
