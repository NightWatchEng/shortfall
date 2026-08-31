// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package stripe

import (
	"errors"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v79"

	"github.com/NightWatchEng/shortfall/biz"
)

// ClientSource labels outcomes produced from synchronous API responses.
const ClientSource = "stripe:client"

// ProviderCall is one observed Stripe API call, for biz_provider_calls_total.
// Outcome is "success" when Stripe returned a definitive answer (2xx or a 4xx
// business response — the API was reachable) and "failed" when it did not (a
// transport error/timeout, a 5xx, or a 429) — the provider-health view.
type ProviderCall struct {
	Op         string
	Outcome    string
	StatusCode int // 0 for a transport error/timeout
	Latency    time.Duration
	Err        error
}

// Backend decorates a stripe.Backend: it observes every API call for
// biz_provider_calls_total and, when an auth-stage call (a PaymentIntent
// create/confirm) fails at the infrastructure level — a timeout or 5xx that
// Stripe never sends a webhook for — emits an auth/failed outcome so that
// synchronous, webhook-invisible loss is still counted.
//
// Business declines (a 4xx such as a card decline) are Stripe answering
// correctly; those arrive on the response and are the caller's/webhook's
// domain, not a provider failure.
type Backend struct {
	stripe.Backend // embedded: CallStreaming/CallRaw/CallMultipart/SetMaxNetworkRetries delegate
	onCall         func(ProviderCall)
	onAuth         func(biz.Outcome)
	now            func() time.Time
}

// BackendOption configures WrapBackend.
type BackendOption func(*Backend)

// WithProviderMetric registers a callback for every call. Hand it to the
// emitter and the call lands on biz_provider_calls_total{provider,op,outcome}
// — inside the buffer, the label fence and the drop counter:
//
//	WithProviderMetric(func(p ProviderCall) {
//		em.RecordProviderCall("stripe", p.Op, p.Outcome)
//	})
//
// ProviderCall.Outcome already uses emit's success/failed spelling.
func WithProviderMetric(f func(ProviderCall)) BackendOption {
	return func(b *Backend) { b.onCall = f }
}

// WithAuthOutcome registers a callback for the synchronous auth-stage failures
// Stripe never webhooks (timeouts / 5xx on a PaymentIntent create/confirm).
func WithAuthOutcome(f func(biz.Outcome)) BackendOption {
	return func(b *Backend) { b.onAuth = f }
}

func withClock(now func() time.Time) BackendOption {
	return func(b *Backend) { b.now = now }
}

// WrapBackend wraps inner. Set it as Stripe's backend
// (stripe.SetBackend(stripe.APIBackend, wrapped)) so every API call is observed.
func WrapBackend(inner stripe.Backend, opts ...BackendOption) *Backend {
	b := &Backend{Backend: inner, now: time.Now}
	for _, o := range opts {
		o(b)
	}

	return b
}

// Call observes one API call, then delegates.
func (b *Backend) Call(method, path, key string, params stripe.ParamsContainer, v stripe.LastResponseSetter) error {
	start := b.now()
	err := b.Backend.Call(method, path, key, params, v)
	latency := b.now().Sub(start)

	status := statusOf(err)
	outcome := providerOutcome(status, err)
	op := deriveOp(method, path)

	if b.onCall != nil {
		b.onCall(ProviderCall{Op: op, Outcome: outcome, StatusCode: status, Latency: latency, Err: err})
	}

	if b.onAuth != nil && outcome == "failed" && isAuthOp(op) {
		if vc, ok := authVC(params); ok {
			b.onAuth(biz.Outcome{
				At: start, Stage: "auth", Result: biz.ResultFailed,
				Source: ClientSource, Err: truncErr(err), VC: vc,
			})
		}
	}

	return err
}

// statusOf returns the HTTP status: 200 on success, the *stripe.Error status
// when Stripe answered, or 0 for a transport error/timeout.
func statusOf(err error) int {
	if err == nil {
		return 200
	}

	var se *stripe.Error
	if errors.As(err, &se) {
		return se.HTTPStatusCode
	}

	return 0
}

// providerOutcome classifies a call for provider health: reachable-and-answered
// ("success") vs no definitive answer ("failed": transport, 5xx, or 429).
func providerOutcome(status int, err error) string {
	if err == nil {
		return "success"
	}

	if status >= 400 && status < 500 && status != 429 {
		return "success" // Stripe answered with a business/client error
	}

	return "failed"
}

// isAuthOp reports whether op is a synchronous auth-stage operation.
func isAuthOp(op string) bool {
	return op == "payment_intents.create" || op == "payment_intents.confirm"
}

// authVC extracts the ValueContext (flow/entity/customer + money) from a
// PaymentIntent params object's metadata and amount. ok is false when the
// params are not a PaymentIntent or carry no biz metadata.
func authVC(params stripe.ParamsContainer) (biz.ValueContext, bool) {
	pi, ok := params.(*stripe.PaymentIntentParams)
	if !ok || pi == nil {
		return biz.ValueContext{}, false
	}

	// PaymentIntentParams carries its own (non-deprecated) Metadata field,
	// which is where WithStripeMetadata's AddMetadata wrote and what Stripe
	// serializes — read it directly, not the embedded base Params.Metadata.
	md := pi.Metadata
	flow := md[MetaFlow]
	if flow == "" {
		return biz.ValueContext{}, false
	}

	var amount int64
	var currency string
	if pi.Amount != nil {
		amount = *pi.Amount
	}

	if pi.Currency != nil {
		currency = strings.ToUpper(*pi.Currency)
	}

	return biz.ValueContext{
		Flow: flow, EntityID: md[MetaEntity], CustomerID: md[MetaCustomer],
		Money: biz.Money{Amount: amount, Currency: currency, Exponent: currencyExponent(currency)},
		Kind:  biz.KindGMV,
	}, true
}

// stripeNamespaces are the Stripe path prefixes that group resources rather
// than being a resource themselves — /v1/<ns>/<resource>/<id>. They shift the
// id one segment to the right, so deriveOp must know them to strip ids by
// position. The set is small and changes rarely; if Stripe adds one we miss,
// the fallout is a mislabeled (still bounded) op, never a leaked id — the
// isIDShaped backstop drops the auto-generated ids these namespaces always use.
var stripeNamespaces = map[string]bool{
	"checkout": true, "billing_portal": true, "billing": true, "issuing": true,
	"treasury": true, "radar": true, "terminal": true, "financial_connections": true,
	"identity": true, "reporting": true, "apps": true, "test_helpers": true,
	"climate": true, "forwarding": true, "tax": true, "entitlements": true, "sigma": true,
}

// deriveOp turns (method, path) into a bounded op label (ADR-0004) by stripping
// Stripe object ids. Ids are stripped by position, not shape: the segment after
// a resource is always an id — including user-defined ones (a coupon id
// "summer", a custom product id) that look exactly like a resource word, which
// no shape test could catch. Examples:
//
//	POST /v1/payment_intents                  -> payment_intents.create
//	POST /v1/payment_intents/pi_X/confirm     -> payment_intents.confirm
//	GET  /v1/coupons/summer                   -> coupons.get      (id stripped)
//	GET  /v1/checkout/sessions/cs_X           -> checkout.sessions.get
//	GET  /v1/customers/cus_X/sources/card_Y   -> customers.sources.get
func deriveOp(method, path string) string {
	segs := make([]string, 0, 5)
	for _, s := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if s != "" && s != "v1" {
			segs = append(segs, s)
		}
	}

	if len(segs) == 0 {
		return strings.ToLower(method)
	}

	// Resource span: two segments for a known namespace, else one.
	var parts []string
	i := 1
	if len(segs) >= 2 && stripeNamespaces[segs[0]] {
		parts = []string{segs[0], segs[1]}
		i = 2
	} else {
		parts = []string{segs[0]}
	}

	// From here segments alternate id, subresource, id, … A subresource/action
	// word is kept; the id after it is stripped by position (or, for an
	// unknown namespace, by shape). endedAtID tracks whether the path stops on
	// an id (a single-resource op) vs a trailing name.
	idSeen, endedAtID := false, false
	for i < len(segs) {
		// segs[i] is an id position — drop it.
		i, idSeen, endedAtID = i+1, true, true
		if i < len(segs) {
			if s := segs[i]; !isIDShaped(s) { // backstop: never keep an id-shaped token
				parts = append(parts, s)
			}

			i, endedAtID = i+1, false
		}
	}

	base := strings.Join(parts, ".")
	switch {
	case endedAtID:
		// Path ends on an id: a single-resource op.
		switch method {
		case "GET":
			return base + ".get"
		case "POST":
			return base + ".update"
		case "DELETE":
			return base + ".delete"
		}
	case idSeen:
		// A name after an id (confirm/capture/sources/…) is already in base.
		return base
	default:
		// /<resource> with no id: a collection op.
		switch method {
		case "POST":
			return base + ".create"
		case "GET":
			return base + ".list"
		case "DELETE":
			return base + ".delete"
		}
	}

	return base + "." + strings.ToLower(method)
}

// isIDShaped is the boundedness backstop for the unknown-namespace case: it
// reports whether a segment looks like an auto-generated Stripe id
// (prefix_suffix with a digit or uppercase letter in the suffix, e.g.
// cs_test_ABC123). Position, not shape, is the primary id test — user-defined
// ids ("summer") are shapeless; this only stops an auto-generated id from an
// unknown namespace from reaching the op label.
func isIDShaped(s string) bool {
	i := strings.IndexByte(s, '_')
	if i <= 0 || i == len(s)-1 {
		return false
	}

	for _, r := range s[i+1:] {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}

	return false
}

// truncErr renders an error for the outcome's Err field, capped so a verbose
// SDK error cannot blow the 512-byte outcome limit.
func truncErr(err error) string {
	if err == nil {
		return ""
	}

	s := err.Error()
	if len(s) > 480 {
		s = s[:480]
	}

	return s
}
