package statsd

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/emit"
)

var at = time.Date(2026, 8, 27, 17, 0, 0, 0, time.UTC)

func valueLbls(cur string) map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "outcome": "failed", "currency": cur, "kind": "fee", "segment": "smb"}
}
func inflightLbls() map[string]string {
	return map[string]string{"flow": "invoice.pay", "stage": "capture", "age_bucket": "5m-30m", "currency": "USD"}
}

func lines(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestEncodeWireFormats(t *testing.T) {
	cases := []struct {
		name   string
		format Format
		point  emit.MetricPoint
		want   string
	}{
		{
			name:   "dogstatsd counter with sorted tags",
			format: DogStatsD,
			point:  emit.MetricPoint{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
			want:   "biz_value_total:14900|c|#currency:USD,flow:invoice.pay,kind:fee,outcome:failed,segment:smb,stage:capture",
		},
		{
			name:   "dogstatsd gauge",
			format: DogStatsD,
			point:  emit.MetricPoint{Name: "biz_inflight_value", Labels: inflightLbls(), Value: 5568661, At: at},
			want:   "biz_inflight_value:5568661|g|#age_bucket:5m-30m,currency:USD,flow:invoice.pay,stage:capture",
		},
		{
			name:   "plain statsd name-encodes values positionally",
			format: PlainStatsD,
			point:  emit.MetricPoint{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
			want:   "biz_value_total.invoice_pay.capture.failed.USD.fee.smb:14900|c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e, err := New(WithWriter(&buf), WithFormat(c.format), WithLogger(slog.New(slog.NewTextHandler(&buf2{}, nil))))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{c.point}); err != nil {
				t.Fatal(err)
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := lines(t, buf.Bytes())
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("line = %q, want %q", got, c.want)
			}
		})
	}
}

// buf2 is a throwaway sink for the warning logger so log output does not
// pollute the metric buffer.
type buf2 struct{}

func (buf2) Write(p []byte) (int, error) { return len(p), nil }

// TestSanitizationBlocksInjection feeds StatsD framing bytes inside a label
// value and asserts they cannot break line framing or inject a second
// metric, an extra tag, a type change, or a sample rate — in either wire
// format. This is the injection defense; a regression that stopped stripping
// a byte must fail here.
func TestSanitizationBlocksInjection(t *testing.T) {
	// A value packed with every reserved byte for both formats.
	evil := "a\nb|c,d:e#f@g h.i"
	cases := []struct {
		name   string
		format Format
		// forbidden bytes must not survive anywhere in the emitted line
		// beyond the framing the exporter itself writes.
	}{
		{name: "dogstatsd", format: DogStatsD},
		{name: "plain", format: PlainStatsD},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e, _ := New(WithWriter(&buf), WithFormat(c.format), WithLogger(slog.New(slog.NewTextHandler(&buf2{}, nil))))
			lbls := valueLbls("USD")
			lbls["segment"] = evil // inject into one label value
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
				{Name: "biz_value_total", Labels: lbls, Value: 14900, At: at},
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := lines(t, buf.Bytes())
			// Exactly one metric line — no injected second metric via newline.
			if len(got) != 1 {
				t.Fatalf("injection produced %d lines, want 1: %q", len(got), got)
			}
			line := got[0]
			// The value's reserved bytes must be gone (replaced with '_').
			for _, b := range []string{"\n", "\r", "\t", " "} {
				if strings.Contains(line, b) {
					t.Fatalf("whitespace/newline survived sanitization: %q", line)
				}
			}
			// The evil substring's framing bytes must be gone. DogStatsD tags
			// permit '.', so only the framing bytes are stripped there; plain
			// name-encoding also strips '.' (a segment separator).
			if c.format == PlainStatsD {
				want := "a_b_c_d_e_f_g_h_i" // '.' also -> '_'
				if !strings.Contains(line, want) {
					t.Fatalf("plain value not fully sanitized in %q (want %q)", line, want)
				}
			} else {
				want := "segment:a_b_c_d_e_f_g_h.i" // '.' kept, framing stripped
				if !strings.Contains(line, want) {
					t.Fatalf("dogstatsd value not fully sanitized in %q (want %q)", line, want)
				}
			}
			// Structural integrity: exactly one type marker and, for dogstatsd,
			// exactly one tag section.
			if strings.Count(line, "|c") != 1 {
				t.Fatalf("type marker count wrong (injection?): %q", line)
			}
			if c.format == DogStatsD && strings.Count(line, "|#") != 1 {
				t.Fatalf("tag-section marker count wrong (injection?): %q", line)
			}
		})
	}
}

func TestUnknownFamilyAndNegativeDeltaError(t *testing.T) {
	cases := []struct {
		name  string
		point emit.MetricPoint
	}{
		{"unknown family", emit.MetricPoint{Name: "biz_bogus", Labels: map[string]string{}, Value: 1, At: at}},
		{"negative counter delta", emit.MetricPoint{Name: "biz_value_total", Labels: valueLbls("USD"), Value: -1, At: at}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := New(WithWriter(&bytes.Buffer{}))
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{c.point}); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestPlainStatsDLogsLossWarningOnce(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	e, _ := New(WithWriter(&bytes.Buffer{}), WithFormat(PlainStatsD), WithLogger(logger))
	for i := 0; i < 3; i++ {
		if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
			{Name: "biz_txn_total", Labels: map[string]string{"flow": "f", "stage": "s", "outcome": "o", "currency": "USD", "segment": "smb"}, Value: 1, At: at},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "labels are encoded positionally"); n != 1 {
		t.Fatalf("want exactly 1 lossiness warning, got %d", n)
	}
}

func TestInflightGaugeHonorsAtOrdering(t *testing.T) {
	older := at
	newer := at.Add(time.Minute)
	cases := []struct {
		name  string
		order []struct {
			v  int64
			at time.Time
		}
		wantLast string
	}{
		{"out-of-order drops the stale sample", []struct {
			v  int64
			at time.Time
		}{{6000000, newer}, {5568661, older}}, "biz_inflight_value:6000000|g|#age_bucket:5m-30m,currency:USD,flow:invoice.pay,stage:capture"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			e, _ := New(WithWriter(&buf))
			for _, s := range c.order {
				if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
					{Name: "biz_inflight_value", Labels: inflightLbls(), Value: s.v, At: s.at},
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := lines(t, buf.Bytes())
			// Only the fresh sample is sent; the stale one is dropped.
			if len(got) != 1 || got[0] != c.wantLast {
				t.Fatalf("lines = %q, want only %q", got, c.wantLast)
			}
		})
	}
}

// TestUDPCaptureBothFormats binds a UDP socket and verifies each wire format
// arrives on the wire as one datagram per metric.
func TestUDPCaptureBothFormats(t *testing.T) {
	cases := []struct {
		name   string
		format Format
		want   string
	}{
		{"dogstatsd", DogStatsD, "biz_value_total:14900|c|#currency:USD,flow:invoice.pay,kind:fee,outcome:failed,segment:smb,stage:capture"},
		{"plain", PlainStatsD, "biz_value_total.invoice_pay.capture.failed.USD.fee.smb:14900|c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			addr := conn.LocalAddr().String()

			e, err := New(WithAddress(addr), WithFormat(c.format), WithLogger(slog.New(slog.NewTextHandler(&buf2{}, nil))))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ExportMetrics(context.Background(), []emit.MetricPoint{
				{Name: "biz_value_total", Labels: valueLbls("USD"), Value: 14900, At: at},
			}); err != nil {
				t.Fatal(err)
			}

			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 2048)
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				t.Fatalf("no datagram captured: %v", err)
			}
			if got := string(buf[:n]); got != c.want {
				t.Fatalf("datagram = %q, want %q", got, c.want)
			}
			_ = e.Shutdown(context.Background())
		})
	}
}

func TestBadAddressFailsAtNew(t *testing.T) {
	if _, err := New(WithAddress("not a valid addr")); err == nil {
		t.Fatal("bad address must fail at New")
	}
}
