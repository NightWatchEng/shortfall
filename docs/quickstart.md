# Quickstart

Instrument a service and watch business value come out of it, in about
ten minutes. No external services, no datastore, no collector — the
Prometheus exporter serves the metrics from your own process.

At the end you will have a running service emitting the `biz_*` families
an impact report is computed from. Wiring a real payment path — multiple
stages, queues, propagation across services — is the
[integration guide](integration.md).

## 1. Install

```sh
mkdir shortfall-demo && cd shortfall-demo && go mod init demo
go get github.com/NightWatchEng/shortfall
go get github.com/NightWatchEng/shortfall/adapters/export/prometheus
```

The exporter is a separate module, so you pull only the backend you use.

## 2. Declare the flow

The **registry** is the file that says what counts as money and what a
flow's stages are. Finance co-signs it once; every later number is
measured against it. Save this as `registry.yaml`:

```yaml
version: 1
segments: [smb, enterprise]
flows:
  invoice.pay:
    money: { kind: fee }          # gmv | net_revenue | fee | take_rate
    currencies: [USD]
    stages:
      - { name: auth, signals: ["http:POST /pay"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 1 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "sql:ledger.payments" }
```

That is the smallest document the validator accepts. `baseline`,
`recovery` and `reconcile` are required because the legs that need them
must not silently guess: the baseline is how "demand that never arrived"
gets sized, recovery is what fraction of it comes back, and `reconcile`
names the ledger the trust number is measured against. You are not using
those legs yet — the values above are placeholders you will tune when you
do.

Unknown fields are rejected, so a typo fails rather than silently
defaulting. Every field is in the [registry reference](registry.md).
Check it before you run:

```sh
go run github.com/NightWatchEng/shortfall/cmd/shortfall@latest validate registry.yaml
# registry.yaml: ok — 1 flow(s), 2 segment(s)
```

## 3. Instrument

Three things happen here: load the registry, build an emitter over an
exporter, and attach the business facts to each request before recording
its outcome. Save as `main.go`:

```go
package main

import (
	"context"
	"log"
	"net/http"

	promexport "github.com/NightWatchEng/shortfall/adapters/export/prometheus"
	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/registry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	ctx := context.Background()

	reg, err := registry.Load("registry.yaml")
	if err != nil {
		log.Fatal(err)
	}

	exp, err := promexport.New()
	if err != nil {
		log.Fatal(err)
	}

	em, err := emit.New(&reg, exp)
	if err != nil {
		log.Fatal(err)
	}
	defer em.Close(ctx)

	http.HandleFunc("POST /pay", func(w http.ResponseWriter, r *http.Request) {
		// Attach the business facts where the request enters.
		ctx, err := biz.WithValueContext(r.Context(), biz.ValueContext{
			Flow:       "invoice.pay",
			EntityID:   r.URL.Query().Get("invoice"), // your idempotency key
			CustomerID: "h:c000042",                  // pre-hashed; raw ids are rejected
			Segment:    "smb",
			Money:      biz.Money{Amount: 14900, Currency: "USD", Exponent: 2},
			Kind:       biz.KindFee,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// One Record per stage transition. ?fail=1 takes the failure
		// path, so the demo has both outcomes to show.
		if r.URL.Query().Get("fail") != "" {
			em.Record(ctx, "auth", biz.ResultFailed, emit.WithErr("gateway 502"))
			http.Error(w, "payment failed", http.StatusBadGateway)
			return
		}

		em.Record(ctx, "auth", biz.ResultSuccess)
		w.WriteHeader(http.StatusAccepted)
	})

	http.Handle("/metrics", promhttp.HandlerFor(exp.Gatherer(), promhttp.HandlerOpts{}))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

`EntityID` is the field doing the real work: it is what collapses four
retried failures into one lost payment at report time, so use the
identifier you already treat as the idempotency key.

`Record` never blocks and returns nothing — it cannot fail your request
path. Everything it rejects is counted on `biz_dropped_events_total`
instead of being dropped silently.

## 4. Run it

```sh
go run .                                        # in one terminal
```

```sh
curl -X POST 'localhost:8080/pay?invoice=inv_1'          # succeeds
curl -X POST 'localhost:8080/pay?invoice=inv_2'          # succeeds
curl -X POST 'localhost:8080/pay?invoice=inv_3&fail=1'   # fails

sleep 2                                                  # see the note below
curl -s localhost:8080/metrics | grep '^biz_'
```

**That `sleep` is load-bearing.** `Record` buffers and returns; a
background ticker exports, once a second by default. Scrape immediately
and you get nothing, which is the point — instrumentation that flushed
synchronously would put your telemetry backend in your request path. Call
`em.Flush(ctx)` when you need it now (a Lambda that freezes between
invocations does), or set the cadence with `emit.WithFlushInterval(d)`.

## 5. What you get

```
biz_txn_total{currency="USD",flow="invoice.pay",outcome="failed",segment="smb",stage="auth"} 1
biz_txn_total{currency="USD",flow="invoice.pay",outcome="success",segment="smb",stage="auth"} 2
biz_value_total{currency="USD",flow="invoice.pay",kind="fee",outcome="failed",segment="smb",stage="auth"} 14900
biz_value_total{currency="USD",flow="invoice.pay",kind="fee",outcome="success",segment="smb",stage="auth"} 29800
```

That is the whole idea in four lines: a count **and** a value, split by
outcome, on a fixed label set. `14900` is minor units — $149.00 of fee
that failed. No customer id and no amount ever becomes a label; those
ride the outcome event the emitter produced alongside each metric point,
which is what makes per-entity de-duplication and the customer list
possible at report time.

You can already answer the 3am question with PromQL:

```promql
# failed value per flow, minor units per minute
sum by (flow, currency) (rate(biz_value_total{outcome="failed"}[5m])) * 60
```

## Next

Getting the four-leg impact report out of these signals needs a query
adapter pointed at wherever they land — the CLI reads Prometheus and SQL:

```sh
shortfall impact --registry registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z \
  --flow invoice.pay --prometheus http://prometheus:9090
```

Metrics alone ground the deferred and unrealized legs and a realized
upper bound; the exact, de-duplicated realized figure and the customer
list need the outcome events, so pair a metrics backend with an events
one. Which backend grounds which leg is in
[backends & adapters](adapters.md).

- [Integration guide](integration.md) — the real thing: multiple stages, queue backlog, propagation across services.
- [Money & the legs](money.md) — what each leg means, for Finance.
- [Registry reference](registry.md) — every field.
