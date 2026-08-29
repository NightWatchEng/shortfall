# Quickstart — from `go get` to a first impact report in 10 minutes

This walks from nothing to a rendered `shortfall impact` report against a small
local dataset. No external services required — it uses a SQLite outcomes table
so you can see the whole path end to end.

## 1. Build the CLI (1 min)

The CLI lives in a nested module so the core library stays dependency-light:

```sh
git clone https://github.com/NightWatchEng/shortfall && cd shortfall
go build -o shortfall ./cmd/shortfall
./shortfall version
```

## 2. Point it at a registry (1 min)

The **registry** is the Finance-co-signed YAML that says what counts as money,
what a flow's stages are, and when deferred value becomes lost. A reference one
ships in the repo:

```sh
./shortfall validate registry/testdata/registry.yaml
# registry/testdata/registry.yaml: ok — 1 flow(s), 2 segment(s)
```

See [registry.md](registry.md) for every field.

## 3. Seed a tiny outcomes table (3 min)

**Why SQLite here?** In production, shortfall reads whatever telemetry
backend you already run — your services emit `biz_*` signals through an
export adapter (CloudWatch EMF, Prometheus, OTLP, Datadog, …) and the
engine reads them back through a query adapter. You never install a new
datastore. This walkthrough uses a local SQLite file **only** so the demo
needs zero external services: it stands in for your real event store.
Today's query adapters are `promql` (metrics), and `sql`, `logql`,
`cwinsights`, and `spl` for events — so a CloudWatch shop exports via
`adapters/export/cloudwatch` and reads back through `cwinsights` (see
[adapters.md](adapters.md) for exactly which backend grounds which leg).

Put a few rows in a SQLite table matching the sql adapter's schema:

```sh
sqlite3 demo.db <<'SQL'
CREATE TABLE biz_outcomes (
  flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
  kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER, at INTEGER);
-- two failed captures (realized loss) and one success, during the window
INSERT INTO biz_outcomes VALUES
 ('invoice.pay','capture','failed','USD','smb','fee','h:c1','inv_1',14900, strftime('%s','2026-08-27T14:01:00Z')*1000000000),
 ('invoice.pay','capture','failed','USD','enterprise','fee','h:c2','inv_2',900000, strftime('%s','2026-08-27T14:02:00Z')*1000000000),
 ('invoice.pay','settle','success','USD','smb','fee','h:c3','inv_3',5000,  strftime('%s','2026-08-27T14:03:00Z')*1000000000);
SQL
```

`at` is Unix **nanoseconds**. Amounts are **minor units** (cents for USD).

## 4. Run the impact report (1 min)

```sh
./shortfall impact \
  --registry registry/testdata/registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z \
  --flow invoice.pay \
  --sql "file:demo.db" --sql-driver sqlite \
  --format text
```

You get the four legs, a coverage line, and a suggested severity — each tagged
with the kind of evidence behind it. `--format json` and `--format markdown`
render the same report for pipelines and incident channels.

## 5. (Optional) reconcile for a trust number (2 min)

Coverage reconciles the **captured (success)** value telemetry saw against the
provider's ledger. Given ledger rows (the Stripe reconciler's or a SQL job's
output) as JSON — here one success row matching the demo's settled $50:

```sh
echo '[{"Flow":"invoice.pay","Outcome":"success","Money":{"Amount":5000,"Currency":"USD","Exponent":2},"Count":1}]' > ledger.json
./shortfall reconcile \
  --registry registry/testdata/registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z \
  --flow invoice.pay --sql "file:demo.db" --sql-driver sqlite \
  --ledger ledger.json --source "sql:ledger.payments"
# COVERAGE   [trust] 100.0% reconciled against sql:ledger.payments
```

(Coverage is a `reconcile`-time number — `impact` has no ledger, so its report
shows coverage as unavailable.)

## Wiring it into your own app

The CLI is one consumer of the library. In your services you:

1. Attach business context to requests with `biz.WithValueContext` and let it
   propagate (`propagate/httpmw`, `kafka`, `sqs`, `amqp`).
2. Record every stage transition with `emit.Std.Record(ctx, stage, result, ...)`,
   backed by an exporter (`adapters/export/*`) that ships the `biz_*` metrics
   and events to your backend.
3. Query for a report with `engine.Compute` against a `query.Querier`
   (`adapters/query/promql` for metrics, `adapters/query/sql` for events), or
   just run the CLI.

Next: [adapters.md](adapters.md) (which backend gives which leg),
[money.md](money.md) (what a "dollar" means here), [registry.md](registry.md).
