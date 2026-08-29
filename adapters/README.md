# adapters

Every adapter lives in its own nested Go module (the otel-contrib pattern),
so depending on one backend never pulls another backend's SDK into your
build. Families:

- `export/` — otlp (metrics + events, the vendor-neutral path), prometheus
  (metrics), cloudwatch (EMF: metrics + events), gcp (Cloud Logging events)
- `query/` — promql (metrics), cwinsights, gcplogging and sql (events)
- `payment/` — stripe (webhook receiver, wrapped client, reconciler)
- `incident/` — slack, incidentio, rootly, firehydrant, pagerduty

Each module implements interfaces from the core packages (`emit.Exporter`,
`query.Querier`) and is added to `go.work` when it lands.
