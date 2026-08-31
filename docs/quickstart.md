# Quickstart

Instrument a service and watch business value come out of it, in about
ten minutes. No external services, no datastore, no collector — the
Prometheus exporter serves the metrics from your own process.

At the end you will have a running service emitting the `biz_*` families
an impact report is computed from. Wiring a real payment path — multiple
stages, queues, propagation across services — is the
[integration guide](integration.md).

## 1. Install

**While the repository is private, `go get` cannot resolve it** — the
checksum database returns 404 for a private module, and the one published
tag (`v0.1.0`) predates most of the API below. Clone it and point a
`replace` at your checkout:

```sh
git clone https://github.com/NightWatchEng/shortfall
mkdir shortfall-demo && cd shortfall-demo && go mod init demo

cat >> go.mod <<'EOF'

replace github.com/NightWatchEng/shortfall => ../shortfall

replace github.com/NightWatchEng/shortfall/adapters/export/prometheus => ../shortfall/adapters/export/prometheus
EOF
```

A `replace` says *where* a module is, not that you want it. `go mod tidy`
comes in step 4, once there is code importing them — run it now and it
resolves nothing.

The exporter is a separate module, so you pull only the backend you use —
hence the second `replace`.

Once the repository is public and carries a current tag, this step becomes
the ordinary two lines, and the `replace` directives come out:

```sh
go get github.com/NightWatchEng/shortfall
go get github.com/NightWatchEng/shortfall/adapters/export/prometheus
```

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

`baseline`, `recovery` and `reconcile` are required — the validator
rejects a flow without them — because the legs that need them must not
silently guess: the baseline is how "demand that never arrived"
gets sized, recovery is what fraction of it comes back, and `reconcile`
names the ledger the trust number is measured against. You are not using
those legs yet — the values above are placeholders you will tune when you
do. `currencies` and the second segment are optional; they are here
because a real registry has them.

Unknown fields are rejected, so a typo fails rather than silently
defaulting. Every field is in the [registry reference](registry.md).
Check it before you run:

```sh
(cd ../shortfall && go run ./cmd/shortfall validate ../shortfall-demo/registry.yaml)
# ../shortfall-demo/registry.yaml: ok — 1 flow(s), 2 segment(s)
```

The `cd` is not incidental. `cmd/shortfall` is its own nested module, and
the repo's `go.work` is what stitches the modules together — run it from
the clone root or the CLI will not resolve. (`go run …@latest` cannot
resolve a private module either; that form arrives with the flip.)

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
path. What it *rejects* — an invalid amount, PII in an attribute, a full
buffer — increments `biz_dropped_events_total{reason}` rather than
vanishing. Two paths are quieter: a call the in-process de-dup recognises
as a repeat returns without a counter (that is the point — retries are
not new losses), and a metric-buffer overflow is logged rather than
counted. Send the same `?invoice=` three times and `biz_txn_total` still
reads 1.

## 4. Run it

```sh
go mod tidy      # now that main.go names its imports
go run .         # leave this running
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
that failed. No customer id and no amount is ever a label, by design: an
unbounded label set is a cardinality incident waiting to happen.

They ride the **outcome event** `Record` builds alongside each metric
point instead — and this exporter throws those away. Prometheus has no
place for them, so `adapters/export/prometheus` declares `Events: false`
and keeps that promise. What you are seeing is the metrics half of the
signal. That is enough for the deferred and unrealized legs and a
realized upper bound; the exact de-duplicated realized figure and the
customer list need the events, which means a second exporter — see
[backends & adapters](adapters.md).

You can already answer the 3am question with PromQL:

```promql
# failed value per flow, minor units per minute
sum by (flow, currency) (rate(biz_value_total{outcome="failed"}[5m])) * 60
```

## Next

Getting the four-leg impact report out of these signals needs a query
adapter pointed at wherever they land — the CLI reads Prometheus and SQL.
Run it from the clone, as in step 2:

```sh
(cd ../shortfall && go run ./cmd/shortfall impact \
  --registry ../shortfall-demo/registry.yaml \
  --from 2026-08-27T14:00:00Z --to 2026-08-27T15:00:00Z \
  --flow invoice.pay --prometheus http://prometheus:9090)
```

After the flip, `go install` puts a `shortfall` binary on your PATH and
this is one line.

Metrics alone ground the deferred and unrealized legs and a realized
upper bound; the exact, de-duplicated realized figure and the customer
list need the outcome events, so pair a metrics backend with an events
one. Which backend grounds which leg is in
[backends & adapters](adapters.md).

- [Integration guide](integration.md) — the real thing: multiple stages, queue backlog, propagation across services.
- [Money & the legs](money.md) — what each leg means, for Finance.
- [Registry reference](registry.md) — every field.
