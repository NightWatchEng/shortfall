# What is a "dollar" here — a guide for Finance

shortfall reports dollars, but "a dollar" is ambiguous until you pin three
things: which money definition, whether the money is lost or merely delayed,
and how sure the number is. This page is the shared vocabulary so the incident
channel and the postmortem mean the same thing.

## 1. Which money — `kind`

Every flow declares one money **kind** in the registry. A flow reports under
exactly one, so two flows are never silently added under different definitions.

| kind | what it means | example |
|---|---|---|
| `gmv` | gross merchandise value — the full transaction amount | a $149 order books $149 of GMV |
| `net_revenue` | revenue recognised after refunds/adjustments | the recognised portion of that order |
| `fee` | the fee your business charges on the transaction | a 2.9% processing fee on $149 |
| `take_rate` | the marketplace/platform cut | the platform's slice of the order |

Pick the kind that matches how the flow's owner already reports to Finance. The
number shortfall produces is then directly comparable to their books.

Amounts are always **minor units** (cents for USD, yen for JPY) as integers —
never floats, because float drift is exactly what reconciliation exists to
catch (ADR-0001). Money is never summed across currencies; a report keeps a
separate total per currency.

## 2. Lost vs delayed — the four legs

An incident's dollar impact is not one number. shortfall separates it into legs
so you can act on each correctly:

- **Realized loss** — transactions that terminally failed, de-duplicated so a
  retry storm does not multiply the figure, and net of anything that later
  succeeded. This is the money that is *gone*: the basis for refunds and SLA
  credits.
- **Deferred value** — money in flight or backlogged (queues, retries), by age.
  It is **not lost yet**. The registry's SLA says when a stage's deadline turns
  a delay into loss (`on_breach: lost`) versus merely at risk (`at_risk`). A
  large deferred number during an incident is a warning, not a bill.
- **Unrealized loss** — demand that never happened because the system was
  degraded (abandoned checkouts, upstream suppression). Those transactions do
  not exist in any log, so this is measured against a **baseline** forecast, and
  it is always a **range** (see §3), never a point.
- **Customer impact** — how many distinct accounts were hit, by segment, with
  the top accounts named. The list you call. It is deliberately **gross**:
  an account that failed and then recovered still had a bad experience, so
  this leg is not netted for recovery and its values must not be summed as
  company loss. That is the realized leg's job.

**Never add unrealized into realized.** They answer different questions
(attribution vs counterfactual) and carry different evidence. The report tags
each leg — `deterministic`, `estimate`, or `trust` — for exactly this reason.

## 3. Why the estimate is a range

Realized and deferred are *measured* — the transactions exist and are counted,
so they are point figures tagged `deterministic`. Unrealized is *inferred*: it
is expected volume (the hour-of-week median of the last N weeks; the registry
can name a holiday calendar to exclude, though v0 does not yet apply one)
minus what was observed, valued at the average order value, net of the demand
that comes back later (recovery). Every input has a normal spread, so the
honest output is a band, not a false-precision point. shortfall reports it as
`low … high (mid)` and refuses to collapse it. A range Finance can reason about
beats a point number Finance cannot defend.

## 4. Coverage — why you can trust the number

Telemetry can be incomplete (a dropped exporter, a mis-scoped query). The
**coverage ratio** reconciles the telemetry sums against the provider's own
ledger and reports how much agreed — per (flow, currency), headlined by the
worst-covered slice. A report without a trust line is a claim; the coverage
line is what makes the incident number auditable. A sub-100% figure names
exactly where telemetry and the ledger diverged, so you know whether to trust
the loss number or go fix an exporter first.

## One-sentence version for the incident channel

> Realized = money gone; deferred = money delayed (some will convert to lost at
> its SLA); unrealized = money that never happened, as a range; and coverage =
> how much of this the provider's ledger backs up.
