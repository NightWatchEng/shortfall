package httpmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/baggage"

	"github.com/NightWatchEng/shortfall/biz"
	"github.com/NightWatchEng/shortfall/registry"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r, err := registry.Load("../../registry/testdata/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func vcFixture() biz.ValueContext {
	return biz.ValueContext{
		Flow:       "invoice.pay",
		EntityID:   "inv_777",
		CustomerID: "h:c9",
		Segment:    "smb",
		Money:      biz.Money{Amount: 14900, Currency: "USD", Exponent: 2},
		Kind:       biz.KindFee,
	}
}

func encodedVC(t *testing.T) string {
	t.Helper()
	enc, err := biz.EncodeVC(vcFixture())
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// captureHandler records what the middleware handed downstream.
type captureHandler struct {
	vc  biz.ValueContext
	ok  bool
	err error
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.vc, c.ok, c.err = biz.FromContext(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func TestMiddlewareExtractsValueContext(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		wantOK     bool
		wantEntity string
	}{
		{"valid biz.vc member", "biz.vc=" + encodedVC(t), true, "inv_777"},
		{"absent baggage", "", false, ""},
		{"corrupt member decodes to nothing downstream", "biz.vc=1|garbage", false, ""},
		{"foreign members only", "tenant=acme", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &captureHandler{}
			mw := Middleware(testRegistry(t))(h)
			req := httptest.NewRequest(http.MethodPost, "/pay", nil)
			if c.header != "" {
				req.Header.Set("baggage", c.header)
			}
			mw.ServeHTTP(httptest.NewRecorder(), req)
			if h.ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (err %v)", h.ok, c.wantOK, h.err)
			}
			if c.wantOK && h.vc.EntityID != c.wantEntity {
				t.Fatalf("entity %q", h.vc.EntityID)
			}
		})
	}
}

func TestMiddlewareIngressStamping(t *testing.T) {
	hook := func(r *http.Request) (biz.ValueContext, bool) {
		if r.URL.Path == "/pay" {
			return vcFixture(), true
		}
		return biz.ValueContext{}, false
	}
	cases := []struct {
		name   string
		path   string
		header string
		wantOK bool
		entity string
	}{
		{"unrecognized path not stamped", "/health", "", false, ""},
		{"recognized path stamped", "/pay", "", true, "inv_777"},
		{"existing context wins over the hook", "/pay", "biz.vc=" + mustEncode(t, "inv_existing"), true, "inv_existing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &captureHandler{}
			mw := Middleware(testRegistry(t), WithIngress(hook))(h)
			req := httptest.NewRequest(http.MethodPost, c.path, nil)
			if c.header != "" {
				req.Header.Set("baggage", c.header)
			}
			mw.ServeHTTP(httptest.NewRecorder(), req)
			if h.ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", h.ok, c.wantOK)
			}
			if c.wantOK && h.vc.EntityID != c.entity {
				t.Fatalf("entity %q, want %q", h.vc.EntityID, c.entity)
			}
		})
	}
}

func mustEncode(t *testing.T, entity string) string {
	t.Helper()
	vc := vcFixture()
	vc.EntityID = entity
	enc, err := biz.EncodeVC(vc)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestMiddlewareIngressStampRejectsPII(t *testing.T) {
	hook := func(r *http.Request) (biz.ValueContext, bool) {
		vc := vcFixture()
		vc.EntityID = "4111111111111111" // a PAN: encode is fine, Validate is not
		return vc, true
	}
	h := &captureHandler{}
	mw := Middleware(testRegistry(t), WithIngress(hook))(h)
	req := httptest.NewRequest(http.MethodPost, "/pay", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if h.ok {
		t.Fatal("a PII-carrying stamp reached downstream — the hook output must be validated")
	}
}

// headerRecorder captures the outbound request the transport produced.
type headerRecorder struct {
	got *http.Request
}

func (h *headerRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	h.got = r
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
}

func bagMembers(t *testing.T, header string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if header == "" {
		return out
	}
	bag, err := baggage.Parse(header)
	if err != nil {
		t.Fatalf("outbound baggage unparsable: %v", err)
	}
	for _, m := range bag.Members() {
		out[m.Key()] = true
	}
	return out
}

func TestTransportEgressAllowlist(t *testing.T) {
	ctx, err := biz.WithValueContext(context.Background(), vcFixture())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		url        string
		ctx        context.Context
		preBaggage string
		wantBizVC  bool
		wantOthers []string
	}{
		{"allowed exact host injects", "https://api.example.com/pay", ctx, "", true, nil},
		{"allowed wildcard host injects", "https://payments.internal.example.com/x", ctx, "", true, nil},
		{"allowed host with port injects", "https://api.example.com:8443/pay", ctx, "", true, nil},
		{"disallowed host never sees biz.vc", "https://api.stripe.com/v1", ctx, "", false, nil},
		{"disallowed host gets pre-existing biz.vc STRIPPED", "https://api.stripe.com/v1", context.Background(), "biz.vc=" + encodedVC(t) + ",tenant=acme", false, []string{"tenant"}},
		{"allowed host keeps foreign members", "https://api.example.com/pay", ctx, "tenant=acme", true, []string{"tenant"}},
		{"no vc in ctx, allowed host: nothing to inject", "https://api.example.com/pay", context.Background(), "", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &headerRecorder{}
			tr := NewTransport(testRegistry(t), rec)
			req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.preBaggage != "" {
				req.Header.Set("baggage", c.preBaggage)
			}
			if _, err := tr.RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			members := bagMembers(t, rec.got.Header.Get("baggage"))
			if members["biz.vc"] != c.wantBizVC {
				t.Fatalf("biz.vc present=%v, want %v (header %q)", members["biz.vc"], c.wantBizVC, rec.got.Header.Get("baggage"))
			}
			for _, m := range c.wantOthers {
				if !members[m] {
					t.Fatalf("foreign member %q lost: %q", m, rec.got.Header.Get("baggage"))
				}
			}
		})
	}
}

func TestTransportDoesNotMutateTheOriginalRequest(t *testing.T) {
	ctx, err := biz.WithValueContext(context.Background(), vcFixture())
	if err != nil {
		t.Fatal(err)
	}
	rec := &headerRecorder{}
	tr := NewTransport(testRegistry(t), rec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.example.com/pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("baggage") != "" {
		t.Fatal("RoundTrip mutated the caller's request — net/http forbids it")
	}
	if rec.got.Header.Get("baggage") == "" {
		t.Fatal("clone lost the injection")
	}
}

func TestEndToEndPropagation(t *testing.T) {
	// The acceptance criterion: the example app shape — api stamps at
	// ingress, worker sees the same ValueContext with no worker code
	// beyond the wrapper.
	worker := &captureHandler{}
	workerSrv := httptest.NewServer(Middleware(testRegistry(t))(worker))
	defer workerSrv.Close()

	// The test server's host is 127.0.0.1 — allow it via a registry the
	// test builds (the reference registry's allowlist names example.com).
	regYAML := `
version: 1
segments: [smb, enterprise]
propagation:
  allow_hosts: ["127.0.0.1"]
flows:
  invoice.pay:
    money: { kind: fee }
    stages:
      - { name: auth, signals: ["http:POST /pay"] }
    baseline: { seasonality: hour_of_week, lookback_weeks: 8 }
    recovery: { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "sql:ledger.payments" }
`
	reg, err := registry.Parse([]byte(regYAML))
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: NewTransport(&reg, http.DefaultTransport)}
	ctx, err := biz.WithValueContext(context.Background(), vcFixture())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workerSrv.URL+"/consume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !worker.ok || worker.vc.EntityID != "inv_777" {
		t.Fatalf("worker did not receive the ValueContext: ok=%v vc=%+v", worker.ok, worker.vc)
	}
}

// TestEgressFenceFailsClosed is the regression for two fence bypasses: a
// malformed neighbour or a second header line must never let biz.vc
// reach a disallowed host.
func TestEgressFenceFailsClosed(t *testing.T) {
	valid := encodedVC(t)
	cases := []struct {
		name    string
		lines   []string // baggage field lines (Header.Add each)
		host    string
		wantVC  bool
		wantFor []string // foreign members that must survive
	}{
		{"trailing comma to disallowed host", []string{"biz.vc=" + valid + ","}, "https://api.stripe.com/v1", false, nil},
		{"malformed neighbour to disallowed host", []string{"biz.vc=" + valid + ",bad member"}, "https://api.stripe.com/v1", false, nil},
		{"biz.vc on a second header line to disallowed host", []string{"tenant=acme", "biz.vc=" + valid}, "https://api.stripe.com/v1", false, []string{"tenant"}},
		{"malformed neighbour to ALLOWED host still delivers biz.vc", []string{"biz.vc=" + valid + ",bad member"}, "https://api.example.com/pay", true, nil},
		{"second-line foreign member preserved on rewrite", []string{"biz.vc=" + valid, "tenant=acme"}, "https://api.stripe.com/v1", false, []string{"tenant"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &headerRecorder{}
			tr := NewTransport(testRegistry(t), rec)
			req, err := http.NewRequest(http.MethodPost, c.host, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range c.lines {
				req.Header.Add("baggage", l)
			}
			if _, err := tr.RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			// Inspect every outbound baggage line, not just the first.
			outLines := rec.got.Header.Values("baggage")
			hasVC := false
			foreign := map[string]bool{}
			for _, l := range outLines {
				if bag, err := baggage.Parse(l); err == nil {
					for _, m := range bag.Members() {
						if m.Key() == "biz.vc" {
							hasVC = true
						} else {
							foreign[m.Key()] = true
						}
					}
				}
			}
			// Belt and suspenders: no raw substring of the encoded vc may
			// survive toward a disallowed host, even unparsed.
			if !c.wantVC {
				for _, l := range outLines {
					if strings.Contains(l, "inv_777") || strings.Contains(l, "h:c9") {
						t.Fatalf("biz.vc bytes leaked to %s: %q", c.host, l)
					}
				}
			}
			if hasVC != c.wantVC {
				t.Fatalf("biz.vc present=%v, want %v (lines %v)", hasVC, c.wantVC, outLines)
			}
			for _, m := range c.wantFor {
				if !foreign[m] {
					t.Fatalf("foreign member %q lost: %v", m, outLines)
				}
			}
		})
	}
}

func TestInboundValidVCSurvivesMalformedNeighbour(t *testing.T) {
	// Regression: a valid wire biz.vc plus a malformed neighbour must win
	// over the ingress hook, not be dropped-then-restamped.
	hook := func(r *http.Request) (biz.ValueContext, bool) {
		vc := vcFixture()
		vc.EntityID = "inv_STAMPED"
		return vc, true
	}
	h := &captureHandler{}
	mw := Middleware(testRegistry(t), WithIngress(hook))(h)
	req := httptest.NewRequest(http.MethodPost, "/pay", nil)
	req.Header.Add("baggage", "biz.vc="+encodedVC(t)+",bad member")
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if !h.ok || h.vc.EntityID != "inv_777" {
		t.Fatalf("valid wire biz.vc was not preserved: ok=%v entity=%q", h.ok, h.vc.EntityID)
	}
}

func TestRedirectAcrossTrustBoundaryStrips(t *testing.T) {
	// An allowed host that redirects to a disallowed host: the fence runs
	// per hop, so the second hop must not carry biz.vc.
	var secondHopBaggage []string
	disallowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHopBaggage = r.Header.Values("baggage")
		w.WriteHeader(http.StatusOK)
	}))
	defer disallowed.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, disallowed.URL+"/leak", http.StatusFound)
	}))
	defer allowed.Close()

	// Registry allows only the first server's host.
	allowedHost := strings.Split(strings.TrimPrefix(allowed.URL, "http://"), ":")[0]
	disallowedHost := strings.Split(strings.TrimPrefix(disallowed.URL, "http://"), ":")[0]
	if allowedHost == disallowedHost {
		t.Skip("both httptest servers share a host; cannot exercise cross-boundary redirect")
	}
	regYAML := "version: 1\nsegments: [smb]\npropagation:\n  allow_hosts: [\"" + allowedHost + "\"]\nflows:\n  invoice.pay:\n    money: { kind: fee }\n    stages:\n      - { name: auth, signals: [\"http:POST /pay\"] }\n    baseline: { seasonality: hour_of_week, lookback_weeks: 8 }\n    recovery: { model: usage_loss_curve, recovered_fraction: 0 }\n    reconcile: { source: \"sql:ledger.payments\" }\n"
	reg, err := registry.Parse([]byte(regYAML))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: NewTransport(&reg, http.DefaultTransport)}
	ctx, err := biz.WithValueContext(context.Background(), vcFixture())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, allowed.URL+"/pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, l := range secondHopBaggage {
		if strings.Contains(l, "biz.vc") || strings.Contains(l, "inv_777") {
			t.Fatalf("biz.vc followed the redirect to a disallowed host: %q", secondHopBaggage)
		}
	}
}

func TestIngressEstimatorStampsUnknownAmounts(t *testing.T) {
	// Opt-in: a hook that cannot determine the amount sets Estimated=true
	// and leaves Amount 0; the estimator fills it. A known amount
	// (including a genuine 0) has Estimated=false and is untouched.
	cases := []struct {
		name        string
		segment     string
		flow        string
		inAmount    int64
		inEstimated bool
		wantAmount  int64
		wantEst     bool
	}{
		{"unknown amount opted in, known segment", "enterprise", "invoice.pay", 0, true, 91000, true},
		{"unknown amount opted in, default segment", "smb", "invoice.pay", 0, true, 14200, true},
		{"genuine free checkout is NOT overwritten", "smb", "invoice.pay", 0, false, 0, false},
		{"known amount is left untouched", "smb", "invoice.pay", 500, false, 500, false},
		{"opted-in unregistered flow: no estimator, stays 0 estimated", "smb", "mystery.flow", 0, true, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hook := func(r *http.Request) (biz.ValueContext, bool) {
				return biz.ValueContext{
					Flow:       c.flow,
					EntityID:   "cart_1",
					CustomerID: "h:c",
					Segment:    c.segment,
					Money:      biz.Money{Amount: c.inAmount, Currency: "USD", Exponent: 2},
					Kind:       biz.KindFee,
					Estimated:  c.inEstimated,
				}, true
			}
			h := &captureHandler{}
			mw := Middleware(testRegistry(t), WithIngress(hook))(h)
			req := httptest.NewRequest(http.MethodGet, "/cart", nil)
			mw.ServeHTTP(httptest.NewRecorder(), req)
			if !h.ok {
				t.Fatalf("stamp did not reach downstream")
			}
			if h.vc.Money.Amount != c.wantAmount {
				t.Fatalf("amount = %d, want %d", h.vc.Money.Amount, c.wantAmount)
			}
			if h.vc.Estimated != c.wantEst {
				t.Fatalf("estimated = %v, want %v", h.vc.Estimated, c.wantEst)
			}
		})
	}
}

func TestEstimatorRespectsExponent(t *testing.T) {
	// A JPY/0 flow's estimator must apply its own exponent, not inherit a
	// USD/2 assumption (a 100x error).
	regYAML := `
version: 1
segments: [smb]
propagation:
  allow_hosts: ["api.example.com"]
flows:
  jpy.pay:
    money: { kind: fee }
    stages:
      - { name: auth, signals: ["http:POST /pay"] }
    estimator: { default_minor: 18750, exponent: 0 }
    baseline: { seasonality: hour_of_week, lookback_weeks: 8 }
    recovery: { model: usage_loss_curve, recovered_fraction: 0 }
    reconcile: { source: "sql:ledger.payments" }
`
	reg, err := registry.Parse([]byte(regYAML))
	if err != nil {
		t.Fatal(err)
	}
	hook := func(r *http.Request) (biz.ValueContext, bool) {
		return biz.ValueContext{
			Flow: "jpy.pay", EntityID: "c1", CustomerID: "h:c", Segment: "smb",
			Money: biz.Money{Amount: 0, Currency: "JPY", Exponent: 0}, Kind: biz.KindFee,
			Estimated: true,
		}, true
	}
	h := &captureHandler{}
	mw := Middleware(&reg, WithIngress(hook))(h)
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/cart", nil))
	if h.vc.Money.Exponent != 0 || h.vc.Money.Amount != 18750 {
		t.Fatalf("estimator exponent not applied: %+v", h.vc.Money)
	}
}

func TestEstimatorDoesNotOverrideWireContext(t *testing.T) {
	// A valid biz.vc on the wire wins; the estimator only ever fires on
	// the ingress-stamp path.
	wire := vcFixture()
	wire.Money.Amount = 0 // unknown on the wire, but present and valid
	enc, err := biz.EncodeVC(wire)
	if err != nil {
		t.Fatal(err)
	}
	hook := func(r *http.Request) (biz.ValueContext, bool) {
		t.Fatal("hook must not run when wire context is present")
		return biz.ValueContext{}, false
	}
	h := &captureHandler{}
	mw := Middleware(testRegistry(t), WithIngress(hook))(h)
	req := httptest.NewRequest(http.MethodGet, "/cart", nil)
	req.Header.Set("baggage", "biz.vc="+enc)
	mw.ServeHTTP(httptest.NewRecorder(), req)
	if !h.ok || h.vc.Money.Amount != 0 || h.vc.Estimated {
		t.Fatalf("wire context was altered by the estimator: %+v", h.vc)
	}
}
