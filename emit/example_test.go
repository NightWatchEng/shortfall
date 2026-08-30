package emit_test

import (
	"context"
	"fmt"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
	"github.com/NightWatchEng/shortfall/registry"
)

// captureExporter is the smallest possible Exporter: real services use an
// adapter (adapters/export/prometheus or cloudwatch) instead.
type captureExporter struct {
	events  int
	metrics int
}

func (c *captureExporter) ExportMetrics(_ context.Context, batch []emit.MetricPoint) error {
	c.metrics += len(batch)
	return nil
}

func (c *captureExporter) ExportEvents(_ context.Context, batch []biz.Outcome) error {
	c.events += len(batch)
	return nil
}

func (c *captureExporter) Capabilities() emit.Caps        { return emit.Caps{} }
func (c *captureExporter) Shutdown(context.Context) error { return nil }

// Record every stage transition. The emitter turns each into one unsampled
// outcome event plus bounded metric points; nothing here depends on trace
// sampling, and no caller string can mint an unbounded label.
func Example() {
	reg, err := registry.Parse([]byte(`
version: 1
segments: [smb]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 4 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.0 }
    reconcile: { source: "sql:ledger.payments" }
`))
	if err != nil {
		panic(err)
	}

	exp := &captureExporter{}
	em, err := emit.New(&reg, exp, emit.WithFlushInterval(0)) // caller-driven flush
	if err != nil {
		panic(err)
	}

	ctx, _ := biz.WithValueContext(context.Background(), biz.ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_00000042",
		CustomerID: "h:c000007",
		Segment:    "smb",
		Money:      biz.Money{Amount: 4999, Currency: "USD", Exponent: 2},
		Kind:       biz.KindFee,
	})
	em.Record(ctx, "auth", biz.ResultSuccess)

	_ = em.Close(context.Background()) // flushes what is pending
	fmt.Printf("events: %d, metric points: %d\n", exp.events, exp.metrics)
	// Output: events: 1, metric points: 2
}

// Wire a payment adapter's per-call observation to the provider-health
// counter. adapters/payment/stripe hands every observed API call to a
// WithProviderMetric callback; this is the one line that turns those into
// biz_provider_calls_total{provider, op, outcome}, the counter
// engine.Compute reads to tell an upstream provider failure from internal
// suppression. Going through the emitter (rather than building a
// MetricPoint by hand) is what keeps the call inside the buffer, the
// ADR-0004 label fence and the drop counter.
func ExampleEmitter_RecordProviderCall() {
	reg, err := registry.Parse([]byte(`
version: 1
segments: [smb]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD]
    stages:
      - { name: auth,    signals: ["http:POST /pay"] }
      - { name: capture, signals: ["queue:capture.q"] }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 4 }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.0 }
    reconcile: { source: "sql:ledger.payments" }
`))
	if err != nil {
		panic(err)
	}

	exp := &captureExporter{}
	em, err := emit.New(&reg, exp, emit.WithFlushInterval(0))
	if err != nil {
		panic(err)
	}

	// In a real service this is the callback body:
	//
	//	stripe.WrapBackend(b, stripe.WithProviderMetric(func(p stripe.ProviderCall) {
	//		em.RecordProviderCall("stripe", p.Op, p.Outcome)
	//	}))
	//
	// Op and Outcome are adapter-supplied constants, never request data.
	em.RecordProviderCall("stripe", "capture", emit.ProviderCallSuccess)
	em.RecordProviderCall("stripe", "capture", emit.ProviderCallFailed)

	_ = em.Close(context.Background())
	fmt.Printf("events: %d, metric points: %d\n", exp.events, exp.metrics)
	// Output: events: 0, metric points: 2
}
