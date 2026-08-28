// Package stripe adapts Stripe to shortfall. This file is the INBOUND path: a
// webhook receiver that verifies the signature and maps events to biz.Outcome.
// The wrapped client — synchronous auth-stage outcomes from Stripe API
// responses (the 5xx/timeouts webhooks never report) plus
// biz_provider_calls_total — is the second part of this milestone (M5.6.2 pt2)
// and is not in this module yet. It is a nested module — a non-Stripe user
// never pulls stripe-go.
//
// Signature verification is not optional and not hand-rolled: every webhook
// payload goes through stripe-go's webhook.ConstructEvent, which checks the
// Stripe-Signature HMAC and timestamp tolerance. An unverified payload is
// rejected, never mapped — a forged "payment_failed" must not be able to
// invent a loss.
//
// The ValueContext (flow, entity, customer) rides Stripe metadata: stamp it at
// PaymentIntent creation with WithStripeMetadata so every resulting webhook
// arrives pre-tagged. Amounts and currency come from the event payload
// (Stripe amounts are already minor units); the event's own Created time is
// the outcome time, so a webhook delivered late during an incident does not
// move realized loss into the wrong window.
package stripe

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/webhook"

	"github.com/NightWatchEng/shortfall/biz"
)

// Metadata keys carrying the ValueContext through Stripe.
const (
	MetaFlow     = "biz_flow"
	MetaEntity   = "biz_entity"
	MetaCustomer = "biz_customer"
)

// Source labels every outcome this adapter produces from a webhook.
const Source = "stripe:webhook"

// MetadataSetter is any Stripe params object that can carry metadata —
// *stripe.Params and every resource params type that embeds it (e.g.
// *stripe.PaymentIntentParams, *stripe.InvoiceParams) satisfy it.
type MetadataSetter interface {
	AddMetadata(key, value string)
}

// WithStripeMetadata stamps the ValueContext onto a Stripe params object at
// creation time, so every webhook Stripe later sends for that object carries
// biz_flow/biz_entity/biz_customer and arrives pre-tagged. Only the
// events-only fields ride here — amounts/currency come back on the payload,
// never duplicated into metadata.
func WithStripeMetadata(p MetadataSetter, vc biz.ValueContext) {
	if p == nil {
		return
	}
	p.AddMetadata(MetaFlow, vc.Flow)
	p.AddMetadata(MetaEntity, vc.EntityID)
	p.AddMetadata(MetaCustomer, vc.CustomerID)
}

// mapping records how one Stripe event type becomes an outcome: the stage and
// result, and which payload field carries the amount.
type mapping struct {
	stage  string
	result biz.Result
	amount amountField
}

type amountField int

const (
	amtAmount amountField = iota // "amount" (PaymentIntent, Charge, Dispute)
	amtPaid                      // "amount_paid" (Invoice paid)
	amtDue                       // "amount_due" (Invoice payment_failed)
)

// eventMap is the fixed set of events this adapter understands. An event
// outside it is verified but not mapped (the caller ignores it), never an
// error — Stripe sends many events a business-impact adapter does not care
// about.
var eventMap = map[stripe.EventType]mapping{
	stripe.EventTypePaymentIntentSucceeded:      {"capture", biz.ResultSuccess, amtAmount},
	stripe.EventTypePaymentIntentPaymentFailed:  {"capture", biz.ResultFailed, amtAmount},
	stripe.EventTypePaymentIntentProcessing:     {"capture", biz.ResultDeferred, amtAmount},
	stripe.EventTypePaymentIntentRequiresAction: {"auth", biz.ResultDeferred, amtAmount},
	stripe.EventTypeChargeFailed:                {"capture", biz.ResultFailed, amtAmount},
	stripe.EventTypeInvoicePaid:                 {"settle", biz.ResultSuccess, amtPaid},
	stripe.EventTypeInvoicePaymentFailed:        {"settle", biz.ResultFailed, amtDue},
	stripe.EventTypeChargeDisputeCreated:        {"dispute", biz.ResultFailed, amtAmount},
}

// object captures the payload fields any mapped event carries.
type object struct {
	Amount     int64             `json:"amount"`
	AmountPaid int64             `json:"amount_paid"`
	AmountDue  int64             `json:"amount_due"`
	Currency   string            `json:"currency"`
	Metadata   map[string]string `json:"metadata"`
}

// VerifyAndMap verifies the Stripe-Signature over payload against secret and,
// on success, maps the event to an outcome. The bool is false for a verified
// event this adapter does not map (ignore it). A signature/timestamp failure
// returns a non-nil error and NO outcome — the payload is rejected.
func VerifyAndMap(payload []byte, sigHeader, secret string) (biz.Outcome, bool, error) {
	// The HMAC + timestamp-tolerance check (the security guarantee) still runs;
	// IgnoreAPIVersionMismatch only relaxes the SDK's insistence that the
	// event's api_version equals the pinned stripe-go version. We read only
	// stable primitive fields (amount, currency, metadata), so an account on a
	// different API version deserializes identically — rejecting it would drop
	// legitimate webhooks, not forged ones.
	event, err := webhook.ConstructEventWithOptions(payload, sigHeader, secret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return biz.Outcome{}, false, fmt.Errorf("stripe: signature verification failed: %w", err)
	}
	m, ok := eventMap[event.Type]
	if !ok {
		return biz.Outcome{}, false, nil // verified, but not an event we map
	}
	var obj object
	if event.Data != nil && len(event.Data.Raw) > 0 {
		if err := json.Unmarshal(event.Data.Raw, &obj); err != nil {
			return biz.Outcome{}, false, fmt.Errorf("stripe: decode %s payload: %w", event.Type, err)
		}
	}

	amount := obj.Amount
	switch m.amount {
	case amtPaid:
		amount = obj.AmountPaid
	case amtDue:
		amount = obj.AmountDue
	}
	currency := strings.ToUpper(obj.Currency)

	out := biz.Outcome{
		At:     time.Unix(event.Created, 0).UTC(),
		Stage:  m.stage,
		Result: m.result,
		Source: Source,
		VC: biz.ValueContext{
			Flow:       obj.Metadata[MetaFlow],
			EntityID:   obj.Metadata[MetaEntity],
			CustomerID: obj.Metadata[MetaCustomer],
			Money:      biz.Money{Amount: amount, Currency: currency, Exponent: currencyExponent(currency)},
			Kind:       biz.KindGMV,
		},
	}
	return out, true, nil
}

// currencyExponent returns the ISO-4217 minor-unit exponent for the currencies
// Stripe treats specially; everything else is the 2-decimal default. Stripe
// amounts are already in minor units, so this only sets how the engine reads
// them back as major units.
func currencyExponent(currency string) int8 {
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "JPY", "KMF", "KRW", "MGA", "PYG", "RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "JOD", "KWD", "OMR", "TND":
		return 3
	default:
		return 2
	}
}
