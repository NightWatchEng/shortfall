package eventline

import (
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/biz"
)

// TestParse pins the read side of the biz.* line schema: the decoder must
// accept an EMF event record plus the wire shapes since-removed exporters
// (Loki, Splunk HEC) already ingested, and reject lines that are not
// outcome events.
func TestParse(t *testing.T) {
	at := time.Date(2026, 8, 25, 9, 5, 0, 0, time.UTC)
	full := biz.Outcome{
		At:     at,
		Stage:  "capture",
		Result: biz.ResultFailed,
		Source: "harness",
		Err:    "card_declined",
		VC: biz.ValueContext{
			Flow: "invoice.pay", EntityID: "inv_00000042", CustomerID: "h:c000007",
			Segment: "smb", Kind: biz.KindFee,
			Money: biz.Money{Amount: 14900, Currency: "USD", Exponent: 2},
		},
	}
	cases := []struct {
		name    string
		line    string
		want    biz.Outcome
		wantErr string
	}{
		{
			name: "loki line shape",
			line: `{"biz.flow":"invoice.pay","biz.stage":"capture","biz.outcome":"failed",` +
				`"biz.entity.id":"inv_00000042","biz.customer.id":"h:c000007","biz.amount.minor":14900,` +
				`"biz.amount.currency":"USD","biz.amount.exponent":2,"biz.value.kind":"fee","biz.amount.estimated":false,` +
				`"biz.segment":"smb","source":"harness","error":"card_declined"}`,
			want: full,
		},
		{
			name: "emf event record (extra envelope keys ignored)",
			line: `{"_aws":{"Timestamp":1787967000000},"event":"biz.outcome",` +
				`"biz.flow":"invoice.pay","biz.stage":"capture","biz.outcome":"failed",` +
				`"biz.entity.id":"inv_00000042","biz.customer.id":"h:c000007","biz.amount.minor":14900,` +
				`"biz.amount.currency":"USD","biz.amount.exponent":2,"biz.value.kind":"fee","biz.amount.estimated":false,` +
				`"biz.segment":"smb","source":"harness","error":"card_declined"}`,
			want: full,
		},
		{
			name: "splunk hec event object (source_system alias)",
			line: `{"biz.flow":"invoice.pay","biz.stage":"capture","biz.outcome":"failed",` +
				`"biz.entity.id":"inv_00000042","biz.customer.id":"h:c000007","biz.amount.minor":14900,` +
				`"biz.amount.currency":"USD","biz.amount.exponent":2,"biz.value.kind":"fee","biz.amount.estimated":false,` +
				`"biz.segment":"smb","source_system":"harness","error":"card_declined"}`,
			want: full,
		},
		{
			name: "estimated flag survives",
			line: `{"biz.flow":"invoice.pay","biz.stage":"auth","biz.outcome":"success",` +
				`"biz.amount.minor":18750,"biz.amount.currency":"USD","biz.amount.exponent":2,` +
				`"biz.value.kind":"fee","biz.amount.estimated":true}`,
			want: biz.Outcome{
				At: at, Stage: "auth", Result: biz.ResultSuccess,
				VC: biz.ValueContext{
					Flow: "invoice.pay", Kind: biz.KindFee, Estimated: true,
					Money: biz.Money{Amount: 18750, Currency: "USD", Exponent: 2},
				},
			},
		},
		{
			name:    "not an outcome line",
			line:    `{"level":"info","msg":"gc done"}`,
			wantErr: "not a biz outcome",
		},
		{
			name:    "amount overflows int64 via float",
			line:    `{"biz.flow":"f","biz.outcome":"failed","biz.amount.minor":14900.5}`,
			wantErr: "amount_minor",
		},
		{
			name:    "unparsable json",
			line:    `{"biz.flow":`,
			wantErr: "parse",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse([]byte(c.line), at)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.At != c.want.At || got.Stage != c.want.Stage || got.Result != c.want.Result ||
				got.Source != c.want.Source || got.Err != c.want.Err || got.VC != c.want.VC {
				t.Fatalf("parsed = %+v\nwant     %+v", got, c.want)
			}
		})
	}
}

// TestParseRejectsSkewedLines pins the loud rejections added for money
// honesty: a marked line without an amount would count $0 into sums, and
// an invalid result string would be silently invisible to outcome filters.
func TestParseRejectsSkewedLines(t *testing.T) {
	at := time.Date(2026, 8, 25, 9, 5, 0, 0, time.UTC)
	cases := []struct {
		name, line, wantErr string
	}{
		{
			name:    "marked line without amount",
			line:    `{"biz.flow":"invoice.pay","biz.outcome":"failed","biz.amount.currency":"USD"}`,
			wantErr: "no biz.amount.minor",
		},
		{
			name:    "invalid result string",
			line:    `{"biz.flow":"invoice.pay","biz.outcome":"faild","biz.amount.minor":100}`,
			wantErr: "not a valid result",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.line), at); err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}
