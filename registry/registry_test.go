package registry

import (
	"strings"
	"testing"
	"time"
)

const sampleYAML = `
version: 1
segments: [smb, enterprise]
propagation:
  allow_hosts: ["*.internal.example.com", "api.example.com"]
flows:
  invoice.pay:
    money: { kind: fee }
    currencies: [USD, EUR]
    stages:
      - { name: auth,    signals: ["http:POST /pay", "provider:stripe.payment_intent"] }
      - { name: capture, signals: ["queue:capture.q", "webhook:payment_intent.succeeded"] }
      - { name: settle,  signals: ["queue:settle.q"] }
    sla:
      capture: { deadline: PT30M, on_breach: lost }
      settle:  { deadline: P1D,  on_breach: at_risk }
    estimator: { default_minor: 18750, by_segment: { smb: 14200, enterprise: 91000 } }
    baseline:  { seasonality: hour_of_week, lookback_weeks: 8, holidays: us }
    recovery:  { model: usage_loss_curve, recovered_fraction: 0.6, within: PT2H }
    reconcile: { source: "sql:ledger.payments" }
`

func mustParse(t *testing.T) Registry {
	t.Helper()
	r, err := Parse([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("sample registry rejected: %v", err)
	}
	return r
}

func TestParseSample(t *testing.T) {
	r := mustParse(t)
	f, ok := r.Flow("invoice.pay")
	if !ok {
		t.Fatal("flow invoice.pay missing")
	}
	if f.Money.Kind != "fee" {
		t.Fatalf("kind %q", f.Money.Kind)
	}
	if len(f.Stages) != 3 || f.Stages[1].Name != "capture" {
		t.Fatalf("stages: %+v", f.Stages)
	}
	sla, ok := f.SLA["capture"]
	if !ok || sla.Deadline != 30*time.Minute || sla.OnBreach != BreachLost {
		t.Fatalf("capture sla: %+v", sla)
	}
	if d := f.SLA["settle"].Deadline; d != 24*time.Hour {
		t.Fatalf("settle deadline %v", d)
	}
	if f.Recovery.RecoveredFraction != 0.6 || f.Recovery.Within != 2*time.Hour {
		t.Fatalf("recovery: %+v", f.Recovery)
	}
	if f.Baseline.LookbackWeeks != 8 || f.Baseline.Seasonality != "hour_of_week" {
		t.Fatalf("baseline: %+v", f.Baseline)
	}
	if !r.SegmentValid("smb") || r.SegmentValid("gov") {
		t.Fatal("segment enumeration wrong")
	}
	if !f.StageValid("settle") || f.StageValid("refund") {
		t.Fatal("stage lookup wrong")
	}
}

func TestEstimator(t *testing.T) {
	r := mustParse(t)
	f, _ := r.Flow("invoice.pay")
	if got, ok := f.EstimateMinor("enterprise"); !ok || got != 91000 {
		t.Fatalf("enterprise estimate %d %v", got, ok)
	}
	if got, ok := f.EstimateMinor("unknown-segment"); !ok || got != 18750 {
		t.Fatalf("default estimate %d %v", got, ok)
	}
	if got, ok := f.EstimateMinor(""); !ok || got != 18750 {
		t.Fatalf("empty-segment estimate %d %v", got, ok)
	}
}

func TestISODurations(t *testing.T) {
	cases := []struct {
		s    string
		want time.Duration
		ok   bool
	}{
		{"PT30M", 30 * time.Minute, true},
		{"PT2H", 2 * time.Hour, true},
		{"P1D", 24 * time.Hour, true},
		{"P1DT12H", 36 * time.Hour, true},
		{"PT90S", 90 * time.Second, true},
		{"PT1H30M", 90 * time.Minute, true},
		{"P0D", 0, false}, // zero SLA is a config error
		{"PT0S", 0, false},
		{"30m", 0, false}, // Go syntax is not the registry contract
		{"P", 0, false},
		{"PT", 0, false},
		{"", 0, false},
		{"P1Y", 0, false}, // months/years are ambiguous; rejected
		{"P1M", 0, false},
		{"PT-5M", 0, false},
	}
	for _, c := range cases {
		got, err := ParseISODuration(c.s)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseISODuration(%q) = %v, %v; want %v", c.s, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseISODuration(%q) accepted, want error", c.s)
		}
	}
}

// Each negative fixture must fail with a message naming the offending
// field — "invalid registry" is not actionable at 2am.
func TestNegativeFixtures(t *testing.T) {
	mutate := func(from, to string) string {
		if !strings.Contains(sampleYAML, from) {
			t.Fatalf("fixture mutation source %q not in sample", from)
		}
		return strings.Replace(sampleYAML, from, to, 1)
	}
	cases := []struct {
		name    string
		yaml    string
		wantMsg string
	}{
		{"unknown top-level field", sampleYAML + "\nbogus: 1\n", "bogus"},
		{"bad kind", mutate("kind: fee", "kind: profit"), "kind"},
		{"duplicate stage", mutate("name: settle", "name: auth"), "stage"},
		{"sla names unknown stage", mutate("capture: { deadline: PT30M", "refund: { deadline: PT30M"), "refund"},
		{"bad sla duration", mutate("deadline: PT30M", "deadline: 30minutes"), "deadline"},
		{"bad on_breach", mutate("on_breach: lost", "on_breach: gone"), "on_breach"},
		{"estimator segment outside enumeration", mutate("by_segment: { smb: 14200", "by_segment: { gov: 14200"), "gov"},
		{"non-positive estimator", mutate("default_minor: 18750", "default_minor: 0"), "default_minor"},
		{"bad recovered_fraction", mutate("recovered_fraction: 0.6", "recovered_fraction: 1.5"), "recovered_fraction"},
		{"zero lookback", mutate("lookback_weeks: 8", "lookback_weeks: 0"), "lookback"},
		{"bad currency", mutate("[USD, EUR]", "[USD, euros]"), "curren"},
		{"bad segment name", mutate("[smb, enterprise]", "[smb, Enterprise!]"), "segment"},
		{"empty reconcile source", mutate(`source: "sql:ledger.payments"`, `source: ""`), "reconcile"},
		{"unknown reconcile scheme", mutate("sql:ledger.payments", "ftp:ledger"), "reconcile"},
		{"flow name uppercase", mutate("invoice.pay:", "Invoice.Pay:"), "flow"},
		{"no stages", mutate(`stages:
      - { name: auth,    signals: ["http:POST /pay", "provider:stripe.payment_intent"] }
      - { name: capture, signals: ["queue:capture.q", "webhook:payment_intent.succeeded"] }
      - { name: settle,  signals: ["queue:settle.q"] }`, "stages: []"), "stage"},
		{"stage without signals", mutate(`signals: ["queue:settle.q"]`, "signals: []"), "signal"},
		{"empty allow_hosts entry", mutate(`"api.example.com"`, `""`), "allow_hosts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.wantMsg)) {
				t.Fatalf("error %q does not name %q", err, c.wantMsg)
			}
		})
	}
}

func TestPropagationAllowlist(t *testing.T) {
	r := mustParse(t)
	cases := []struct {
		host string
		ok   bool
	}{
		{"api.example.com", true},
		{"payments.internal.example.com", true},
		{"internal.example.com", false}, // *. requires a subdomain label
		{"api.example.com.evil.io", false},
		{"evilapi.example.com", false},
		{"stripe.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := r.Propagation.HostAllowed(c.host); got != c.ok {
			t.Errorf("HostAllowed(%q) = %v, want %v", c.host, got, c.ok)
		}
	}
}

func TestLoadFromFile(t *testing.T) {
	r, err := Load("testdata/registry.yaml")
	if err != nil {
		t.Fatalf("reference registry rejected: %v", err)
	}
	if _, ok := r.Flow("invoice.pay"); !ok {
		t.Fatal("reference registry missing invoice.pay")
	}
}
