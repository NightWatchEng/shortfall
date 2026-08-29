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
