package httpmw

import (
	"context"
	"net/http"
	"net/http/httptest"
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
