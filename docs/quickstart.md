# Quickstart

From a clone to a rendered impact report in about ten minutes, with no
external services. When you are ready to wire your own service, continue
to the [integration guide](integration.md).

## 1. Build the CLI

The CLI is a nested module, so the core library stays dependency-light.

```sh
git clone https://github.com/NightWatchEng/shortfall && cd shortfall
go build -o shortfall ./cmd/shortfall
./shortfall version
```

## 2. Validate a registry

The **registry** is the YAML that says what counts as money, what a
flow's stages are, and when deferred value becomes lost. Finance
co-signs it once; every later report is measured against it. A reference
registry ships in the repo:

```sh
./shortfall validate registry/testdata/registry.yaml
# registry/testdata/registry.yaml: ok — 1 flow(s), 2 segment(s)
```

Every field is documented in the [registry reference](registry.md).

## 3. Seed an outcomes table

In production the engine reads the telemetry backend you already run.
This walkthrough uses a local SQLite file so the demo needs nothing
installed; it stands in for your event store.

```sh
sqlite3 demo.db <<'SQL'
CREATE TABLE biz_outcomes (
  flow TEXT, stage TEXT, outcome TEXT, currency TEXT, segment TEXT,
  kind TEXT, customer_id TEXT, entity_id TEXT, amount_minor INTEGER, at INTEGER);
-- two failed captures (realized loss) and one success, inside the window
INSERT INTO biz_outcomes VALUES
 ('invoice.pay','capture','failed','USD','smb','fee','h:c1','inv_1',14900, strftime('%s','2026-08-27T14:01:00Z')*1000000000),
 ('invoice.pay','capture','failed','USD','enterprise','fee','h:c2','inv_2',900000, strftime('%s','2026-08-27T14:02:00Z')*1000000000),
 ('invoice.pay','settle','success','USD','smb','fee','h:c3','inv_3',5000,  strftime('%s','2026-08-27T14:03:00Z')*1000000000);
SQL
```

`at` is Unix **nanoseconds**; amounts are **minor units** (cents for USD).

## 4. Run the impact report

```sh
./shortfall impact \
  --registry registry/testdata/registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z \
  --flow invoice.pay \
  --sql "file:demo.db" --sql-driver sqlite \
  --format text
```

You get the four legs, a coverage line and a suggested severity, each
tagged with the kind of evidence behind it. `--format json` and
`--format markdown` render the same report for pipelines and incident
channels.

SQLite is an events-only backend, so the deferred and unrealized legs
report as unavailable with a reason rather than as zero. Pairing a
metrics backend grounds them — see [backends](adapters.md).

## 5. Reconcile for a trust number

Coverage compares the **captured (success)** value telemetry saw against
the provider's ledger. Given one ledger row matching the demo's settled
$50:

```sh
echo '[{"Flow":"invoice.pay","Outcome":"success","Money":{"Amount":5000,"Currency":"USD","Exponent":2},"Count":1}]' > ledger.json
./shortfall reconcile \
  --registry registry/testdata/registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z \
  --flow invoice.pay --sql "file:demo.db" --sql-driver sqlite \
  --ledger ledger.json --source "sql:ledger.payments"
# COVERAGE   [trust] 100.0% reconciled against sql:ledger.payments
```

Coverage is a `reconcile`-time number. An `impact` run carries no
ledger, so it reports coverage as unavailable rather than as 100%.

## Next

- [Integration guide](integration.md) — wire the library into your own service.
- [Backends & adapters](adapters.md) — which backend grounds which leg.
- [Money & the legs](money.md) — what each leg means, for Finance.
