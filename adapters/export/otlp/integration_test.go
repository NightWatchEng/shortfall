// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration test: exercises the real otel OTLP HTTP exporters against a
// live endpoint. It needs an OTLP receiver (a collector) at
// OTEL_EXPORTER_OTLP_ENDPOINT and is excluded from the default build
// (run: go test -tags integration ./...). The unit tests cover the
// mapping; this proves the wire path end to end.
package otlp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

func TestOTLPAgainstLiveCollector(t *testing.T) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		t.Skip("set OTEL_EXPORTER_OTLP_ENDPOINT to a running collector")
	}
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.ExportMetrics(ctx, []emit.MetricPoint{
		{Name: "biz_value_total", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "kind": "fee", "segment": "smb"}, Value: 14900, At: now},
		{Name: "biz_inflight_value", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}, Value: 5568661, At: now},
	}); err != nil {
		t.Fatalf("export metrics: %v", err)
	}
	vc := biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_int", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
	if err := e.ExportEvents(ctx, []biz.Outcome{{At: now, VC: vc, Stage: "capture", Result: biz.ResultFailed, Source: "integration"}}); err != nil {
		t.Fatalf("export events: %v", err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
