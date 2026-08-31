// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Integration test: a LocalStack round-trip for the PutMetricData path. It
// sends the metric families to a LocalStack CloudWatch endpoint, then reads
// them back with ListMetrics. Excluded from the default build; run with:
//
//	LOCALSTACK_ENDPOINT=http://localhost:4566 go test -tags integration ./...
//
// The EMF-to-writer path needs no AWS at all and is covered by the unit
// tests; this proves the optional API path against a real CloudWatch API.
package cloudwatch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"github.com/NightWatchEng/shortfall/emit"
)

func TestPutMetricDataAgainstLocalStack(t *testing.T) {
	endpoint := os.Getenv("LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		t.Skip("set LOCALSTACK_ENDPOINT (e.g. http://localhost:4566) to run")
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := cloudwatch.NewFromConfig(cfg, func(o *cloudwatch.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	ns := "shortfall-it"
	e := New(WithWriter(os.Stdout), WithNamespace(ns), WithMetricPutter(client))
	now := time.Now()
	if err := e.ExportMetrics(ctx, []emit.MetricPoint{
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: now},
		{Name: "biz_txn_total", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "segment": "smb"}, Value: 1, At: now},
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// LocalStack ingests asynchronously; poll ListMetrics briefly.
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := client.ListMetrics(ctx, &cloudwatch.ListMetricsInput{Namespace: aws.String(ns)})
		if err != nil {
			t.Fatalf("list metrics: %v", err)
		}
		seen := map[string]bool{}
		for _, m := range out.Metrics {
			seen[aws.ToString(m.MetricName)] = true
		}
		if seen["biz_value_total"] && seen["biz_txn_total"] {
			return // round-trip confirmed
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics not found in namespace %q within deadline; saw %v", ns, seen)
		}
		time.Sleep(time.Second)
	}
}
