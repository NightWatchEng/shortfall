package cloudwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/emit"
)

var update = flag.Bool("update", false, "update golden files")

var at = time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)

func vc() biz.ValueContext {
	return biz.ValueContext{
		Flow: "invoice.pay", EntityID: "inv_1", CustomerID: "h:c", Segment: "smb",
		Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2}, Kind: biz.KindFee,
	}
}

func valueLbls(cur string) map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": cur, "kind": "fee", "segment": "smb"}
}

// decode splits the buffer into per-line JSON records.
func decode(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("record is not valid JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestExportMetricsEMFShape(t *testing.T) {
	cases := []struct {
		name      string
		point     emit.MetricPoint
		wantErr   bool
		wantDims  []string
		wantValue float64
	}{
		{
			name:      "value_total carries six dimensions",
			point:     emit.MetricPoint{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
			wantDims:  valueDims,
			wantValue: 14900,
		},
		{
			name:      "inflight gauge carries four dimensions",
			point:     emit.MetricPoint{Name: "biz_inflight_value", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}, Value: 5568661, At: at},
			wantDims:  inflightDims,
			wantValue: 5568661,
		},
		{
			name:    "unknown family errors",
			point:   emit.MetricPoint{Name: "biz_bogus", Labels: map[string]string{}, Value: 1, At: at},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := New(WithWriter(&buf))
			err := e.ExportMetrics(context.Background(), []emit.MetricPoint{c.point})
			if c.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			recs := decode(t, buf.Bytes())
			if len(recs) != 1 {
				t.Fatalf("want 1 record, got %d", len(recs))
			}
			r := recs[0]
			if got := r[c.point.Name]; got != c.wantValue {
				t.Fatalf("metric value = %v, want %v", got, c.wantValue)
			}
			aws := r["_aws"].(map[string]any)
			if int64(aws["Timestamp"].(float64)) != at.UnixMilli() {
				t.Fatalf("timestamp = %v, want %d", aws["Timestamp"], at.UnixMilli())
			}
			cwm := aws["CloudWatchMetrics"].([]any)[0].(map[string]any)
			gotDims := toStrings(cwm["Dimensions"].([]any)[0].([]any))
			if !equalStrings(gotDims, c.wantDims) {
				t.Fatalf("dimensions = %v, want %v", gotDims, c.wantDims)
			}
			// Every declared dimension must also be a top-level field (EMF rule).
			for _, d := range c.wantDims {
				if _, ok := r[d]; !ok {
					t.Fatalf("dimension %q missing as top-level field", d)
				}
			}
			// No amount/id may appear as a dimension (ADR-0004).
			for _, d := range gotDims {
				if d == "amount_minor" || d == "entity.id" || d == "customer.id" {
					t.Fatalf("forbidden dimension %q", d)
				}
			}
		})
	}
}

func TestExportEventsCarryAmountsAsFieldsNotMetrics(t *testing.T) {
	var buf bytes.Buffer
	e := New(WithWriter(&buf))
	if err := e.ExportEvents(context.Background(), []biz.Outcome{
		{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed, Source: "stripe:webhook"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := decode(t, buf.Bytes())[0]
	if r["biz.amount_minor"].(float64) != 14900 {
		t.Fatalf("amount = %v", r["biz.amount_minor"])
	}
	if r["biz.entity.id"] != "inv_1" || r["biz.customer.id"] != "h:c" {
		t.Fatalf("ids missing: %v", r)
	}
	// The event record must not declare a metric (no double-count with the
	// metric path): its _aws block has a Timestamp but no CloudWatchMetrics.
	aws := r["_aws"].(map[string]any)
	if _, ok := aws["CloudWatchMetrics"]; ok {
		t.Fatal("event record must not declare metrics")
	}
}

func TestCapabilitiesBothSignals(t *testing.T) {
	e := New(WithWriter(&bytes.Buffer{}))
	if c := e.Capabilities(); !c.Metrics || !c.Events {
		t.Fatalf("caps = %+v", c)
	}
}

// fakePutter records PutMetricData calls.
type fakePutter struct {
	inputs []*cloudwatch.PutMetricDataInput
}

func (f *fakePutter) PutMetricData(_ context.Context, in *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	f.inputs = append(f.inputs, in)
	return &cloudwatch.PutMetricDataOutput{}, nil
}

func TestPutMetricDataReplacesEMFMetricRecords(t *testing.T) {
	fp := &fakePutter{}
	var buf bytes.Buffer
	e := New(WithWriter(&buf), WithMetricPutter(fp))
	ctx := context.Background()
	if err := e.ExportMetrics(ctx, []emit.MetricPoint{
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
		{Name: "biz_txn_total", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "segment": "smb"}, Value: 1, At: at},
	}); err != nil {
		t.Fatal(err)
	}
	if len(fp.inputs) != 1 {
		t.Fatalf("want 1 PutMetricData call, got %d", len(fp.inputs))
	}
	if n := len(fp.inputs[0].MetricData); n != 2 {
		t.Fatalf("want 2 datums, got %d", n)
	}
	// Events still go to the writer even with a putter.
	if err := e.ExportEvents(ctx, []biz.Outcome{{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	recs := decode(t, buf.Bytes())
	// With a putter, no metric EMF records are written (that would
	// double-count); only the one event record reaches the writer.
	if len(recs) != 1 {
		t.Fatalf("want only the event record on the writer, got %d records", len(recs))
	}
	if recs[0]["event"] != "biz.outcome" {
		t.Fatalf("the writer record must be the event, got %v", recs[0])
	}
}

// TestPutMetricDataChunksAtServiceLimit exercises the 1000-datum chunking
// boundary: a batch larger than the limit must split into multiple calls
// with correct sizes and no lost or duplicated datum.
func TestPutMetricDataChunksAtServiceLimit(t *testing.T) {
	cases := []struct {
		name  string
		n     int
		sizes []int
	}{
		{"exactly one chunk", 1000, []int{1000}},
		{"one over the limit", 1001, []int{1000, 1}},
		{"several chunks", 2500, []int{1000, 1000, 500}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := &fakePutter{}
			e := New(WithWriter(&bytes.Buffer{}), WithMetricPutter(fp))
			batch := make([]emit.MetricPoint, c.n)
			for i := range batch {
				batch[i] = emit.MetricPoint{Name: "biz_txn_total", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": "USD", "segment": "smb"}, Value: 1, At: at}
			}
			if err := e.ExportMetrics(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			gotSizes := make([]int, len(fp.inputs))
			total := 0
			for i, in := range fp.inputs {
				gotSizes[i] = len(in.MetricData)
				total += len(in.MetricData)
			}
			if !equalInts(gotSizes, c.sizes) {
				t.Fatalf("chunk sizes = %v, want %v", gotSizes, c.sizes)
			}
			if total != c.n {
				t.Fatalf("total datums = %d, want %d (lost or duplicated)", total, c.n)
			}
		})
	}
}

// TestEMFGolden pins the exact EMF exposition for a fixed batch.
func TestEMFGolden(t *testing.T) {
	var buf bytes.Buffer
	e := New(WithWriter(&buf))
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.ExportMetrics(ctx, []emit.MetricPoint{
		{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
		{Name: "biz_inflight_value", Labels: map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}, Value: 5568661, At: at},
	}))
	must(e.ExportEvents(ctx, []biz.Outcome{{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed, Source: "stripe:webhook"}}))
	must(e.Shutdown(ctx))

	// Re-marshal each record with sorted keys for a stable golden (Go map
	// marshalling already sorts keys, but normalise defensively).
	got := normalizeJSONL(t, buf.Bytes())
	golden := filepath.Join("testdata", "emf.golden")
	if *update {
		must(os.WriteFile(golden, got, 0o644))
	}
	want, err := os.ReadFile(golden)
	must(err)
	if !bytes.Equal(got, want) {
		t.Fatalf("EMF does not match golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func normalizeJSONL(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, r := range decode(t, b) {
		enc, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		out.Write(enc)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func toStrings(a []any) []string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = v.(string)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPostShutdownExportRefused pins the terminal branch of emit.Exporter's
// post-Shutdown contract for both signals and both metric paths: a later
// export returns ErrShutdown and delivers nothing (workspace-0cd).
func TestPostShutdownExportRefused(t *testing.T) {
	var buf bytes.Buffer
	e := New(WithWriter(&buf))
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	flushed := buf.Len()
	p := emit.MetricPoint{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 100, At: at}
	if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{p}); !errors.Is(err, ErrShutdown) {
		t.Fatalf("post-shutdown ExportMetrics err = %v, want ErrShutdown", err)
	}
	o := biz.Outcome{At: at, VC: vc(), Stage: "capture", Result: biz.ResultFailed}
	if err := e.ExportEvents(context.Background(), []biz.Outcome{o}); !errors.Is(err, ErrShutdown) {
		t.Fatalf("post-shutdown ExportEvents err = %v, want ErrShutdown", err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	if buf.Len() != flushed {
		t.Fatalf("post-shutdown export reached the writer: %d bytes appeared", buf.Len()-flushed)
	}
	// Empty batches stay a no-op even after Shutdown: nothing to refuse.
	if err := e.ExportMetrics(context.Background(), nil); err != nil {
		t.Fatalf("post-shutdown empty batch errored: %v", err)
	}

	// The PutMetricData path refuses too, before any API call.
	fake := &fakePutter{}
	ep := New(WithWriter(&bytes.Buffer{}), WithMetricPutter(fake))
	if err := ep.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := ep.ExportMetrics(context.Background(), []emit.MetricPoint{p}); !errors.Is(err, ErrShutdown) {
		t.Fatalf("post-shutdown putter ExportMetrics err = %v, want ErrShutdown", err)
	}
	if got := len(fake.inputs); got != 0 {
		t.Fatalf("post-shutdown export reached PutMetricData: %d calls", got)
	}
}
