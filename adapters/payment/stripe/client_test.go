// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package stripe

import (
	"bytes"
	"errors"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/form"

	"github.com/NightWatchEng/shortfall/biz"
)

// fakeBackend is a stripe.Backend whose Call returns a scripted error.
type fakeBackend struct {
	err error
}

func (f *fakeBackend) Call(_, _, _ string, _ stripe.ParamsContainer, _ stripe.LastResponseSetter) error {
	return f.err
}
func (f *fakeBackend) CallStreaming(_, _, _ string, _ stripe.ParamsContainer, _ stripe.StreamingLastResponseSetter) error {
	return f.err
}
func (f *fakeBackend) CallRaw(_, _, _ string, _ *form.Values, _ *stripe.Params, _ stripe.LastResponseSetter) error {
	return f.err
}
func (f *fakeBackend) CallMultipart(_, _, _, _ string, _ *bytes.Buffer, _ *stripe.Params, _ stripe.LastResponseSetter) error {
	return f.err
}
func (f *fakeBackend) SetMaxNetworkRetries(int64) {}

func TestDeriveOp(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"POST", "/v1/payment_intents", "payment_intents.create"},
		{"POST", "/v1/payment_intents/pi_123/confirm", "payment_intents.confirm"},
		{"GET", "/v1/payment_intents/pi_123", "payment_intents.get"},
		{"POST", "/v1/payment_intents/pi_123", "payment_intents.update"},
		{"DELETE", "/v1/customers/cus_NffrFeUf", "customers.delete"},
		{"GET", "/v1/charges", "charges.list"},
		{"POST", "/v1/refunds", "refunds.create"},
		// Namespaced resources: the id is one segment further right, so a
		// fixed-position parse would leak it. Stripped by namespace-aware position.
		{"GET", "/v1/checkout/sessions/cs_test_abc123", "checkout.sessions.get"},
		{"POST", "/v1/checkout/sessions", "checkout.sessions.create"},
		{"POST", "/v1/checkout/sessions/cs_test_abc123/expire", "checkout.sessions.expire"},
		{"GET", "/v1/billing_portal/sessions/bps_1Ab2", "billing_portal.sessions.get"},
		{"GET", "/v1/issuing/cards/ic_1AbCdefG", "issuing.cards.get"},
		{"GET", "/v1/customers/cus_NffrFeUf/sources/card_1Ab2", "customers.sources.get"},
		// User-defined ids (coupons, custom product/plan ids) are all-lowercase
		// and shape-indistinguishable from resource words — they are stripped
		// by position, never leaked into the op label (the boundedness guard).
		{"GET", "/v1/coupons/summer", "coupons.get"},
		{"GET", "/v1/coupons/FREESHIP", "coupons.get"},
		{"POST", "/v1/coupons/black_friday", "coupons.update"},
		{"DELETE", "/v1/coupons/25off", "coupons.delete"},
		{"GET", "/v1/products/my-custom-product", "products.get"},
		{"GET", "/v1/plans/gold", "plans.get"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if got := deriveOp(c.method, c.path); got != c.want {
				t.Fatalf("deriveOp(%q,%q) = %q, want %q", c.method, c.path, got, c.want)
			}
		})
	}
}

func TestProviderOutcomeClassification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   string
		wantSt int
	}{
		{"success", nil, "success", 200},
		{"transport error -> failed", errors.New("dial timeout"), "failed", 0},
		{"5xx -> failed", &stripe.Error{HTTPStatusCode: 503}, "failed", 503},
		{"429 -> failed", &stripe.Error{HTTPStatusCode: 429}, "failed", 429},
		{"402 decline -> success (API answered)", &stripe.Error{HTTPStatusCode: 402}, "success", 402},
		{"400 -> success (API answered)", &stripe.Error{HTTPStatusCode: 400}, "success", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if st := statusOf(c.err); st != c.wantSt {
				t.Fatalf("statusOf = %d, want %d", st, c.wantSt)
			}
			if o := providerOutcome(statusOf(c.err), c.err); o != c.want {
				t.Fatalf("outcome = %q, want %q", o, c.want)
			}
		})
	}
}

func piParams(amount int64, currency string, withMeta bool) *stripe.PaymentIntentParams {
	p := &stripe.PaymentIntentParams{Amount: stripe.Int64(amount), Currency: stripe.String(currency)}
	if withMeta {
		WithStripeMetadata(p, biz.ValueContext{Flow: "invoice.pay", EntityID: "inv_1", CustomerID: "h:c1"})
	}
	return p
}

func TestBackendObservesEveryCallAndEmitsAuthFailure(t *testing.T) {
	var calls []ProviderCall
	var auths []biz.Outcome
	clk := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	newBackend := func(err error) *Backend {
		return WrapBackend(&fakeBackend{err: err},
			WithProviderMetric(func(p ProviderCall) { calls = append(calls, p) }),
			WithAuthOutcome(func(o biz.Outcome) { auths = append(auths, o) }),
			withClock(func() time.Time { return clk }),
		)
	}

	// 5xx on a PaymentIntent create: provider call failed and an auth/failed
	// outcome the webhook would never report, carrying the params' money+VC.
	calls, auths = nil, nil
	b := newBackend(&stripe.Error{HTTPStatusCode: 503})
	_ = b.Call("POST", "/v1/payment_intents", "", piParams(14900, "usd", true), nil)
	if len(calls) != 1 || calls[0].Op != "payment_intents.create" || calls[0].Outcome != "failed" {
		t.Fatalf("provider call = %+v", calls)
	}
	if len(auths) != 1 {
		t.Fatalf("want 1 auth outcome, got %d", len(auths))
	}
	if auths[0].Stage != "auth" || auths[0].Result != biz.ResultFailed ||
		auths[0].VC.Money.Amount != 14900 || auths[0].VC.Money.Currency != "USD" ||
		auths[0].VC.Flow != "invoice.pay" || auths[0].Source != ClientSource || !auths[0].At.Equal(clk) {
		t.Fatalf("auth outcome = %+v", auths[0])
	}

	// Success: a provider call recorded, no auth failure.
	calls, auths = nil, nil
	b = newBackend(nil)
	_ = b.Call("POST", "/v1/payment_intents", "", piParams(14900, "usd", true), nil)
	if len(calls) != 1 || calls[0].Outcome != "success" {
		t.Fatalf("success call = %+v", calls)
	}
	if len(auths) != 0 {
		t.Fatal("a successful call must emit no auth failure")
	}

	// 402 decline on create: provider call is "success" (API answered), not
	// a synthetic auth failure — the decline arrives on the response.
	calls, auths = nil, nil
	b = newBackend(&stripe.Error{HTTPStatusCode: 402})
	_ = b.Call("POST", "/v1/payment_intents", "", piParams(14900, "usd", true), nil)
	if len(calls) != 1 || calls[0].Outcome != "success" {
		t.Fatalf("402 call = %+v", calls)
	}
	if len(auths) != 0 {
		t.Fatal("a 402 decline is answered by Stripe, not a provider-infra auth failure")
	}

	// 5xx on a non-auth op: provider call recorded, no auth outcome.
	calls, auths = nil, nil
	b = newBackend(&stripe.Error{HTTPStatusCode: 500})
	_ = b.Call("GET", "/v1/charges", "", &stripe.ChargeListParams{}, nil)
	if len(calls) != 1 || calls[0].Op != "charges.list" {
		t.Fatalf("charges call = %+v", calls)
	}
	if len(auths) != 0 {
		t.Fatal("a non-auth op must not emit an auth outcome")
	}

	// 5xx on create without biz metadata: provider call recorded, but no auth
	// outcome (we cannot ground it without the ValueContext).
	calls, auths = nil, nil
	b = newBackend(&stripe.Error{HTTPStatusCode: 503})
	_ = b.Call("POST", "/v1/payment_intents", "", piParams(14900, "usd", false), nil)
	if len(calls) != 1 {
		t.Fatalf("call = %+v", calls)
	}
	if len(auths) != 0 {
		t.Fatal("no biz metadata -> no groundable auth outcome")
	}
}
