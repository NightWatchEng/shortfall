package stripe

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/paymentintent"

	"github.com/NightWatchEng/shortfall/biz"
)

// PaymentIntentPage is one page of a provider payment-intent listing.
type PaymentIntentPage struct {
	Intents []*stripe.PaymentIntent
	HasMore bool // the provider has further pages after this one
}

// PageFunc fetches the payment intents created at or after since, starting
// after the given cursor id ("" for the first page). It returns exactly one
// page and whether more pages follow — Reconcile drives the cursor. A real
// binding over the Stripe SDK is ListPageFunc; tests supply their own.
type PageFunc func(ctx context.Context, since time.Time, startingAfter string) (PaymentIntentPage, error)

// Ledger is the reconciliation result: the aggregated rows the coverage leg
// compares against telemetry, plus how many provider records were scanned and
// how many were skipped (non-terminal or unclassifiable), so a coverage ratio
// can be traced back to what the provider actually reported.
type Ledger struct {
	Rows    []biz.LedgerRow
	Scanned int
	Skipped int
}

// Reconcile pages a provider's payment intents created at/after since and
// aggregates them into ledger rows keyed by (flow, currency, outcome). It
// follows the cursor until the provider reports no more pages; a page with
// HasMore=false — or, defensively, an empty page or a non-advancing cursor —
// ends the walk so a misbehaving pager cannot spin forever. Rows come back in
// a deterministic order (flow, then currency, then outcome).
//
// Outcome mapping is conservative: only terminal facts become rows. A succeeded
// intent is a success; a processing intent is deferred; a canceled or
// payment-method-required intent that carries a payment error is a failed loss.
// Everything still in flight (authorized-awaiting-capture, requires_action, or
// a bare cancel with no error) is skipped, not guessed — counted in Skipped so
// the omission is visible rather than silently zeroed.
//
// Every row's amount is the intent's `amount` (the intended value), NEVER
// `amount_received`. This is deliberate: the webhook telemetry path this ledger
// is reconciled against records a succeeded intent at `amount` too (stripe.go
// eventMap uses the payload's `amount` field), so both sides of the coverage
// comparison measure the same money and a partial capture does not read as a
// telemetry drift. Surfacing captured-vs-intended shortfall on partial captures
// is a separate concern (both sides currently use the intended amount), the
// basis ratified in ADR-0010, not silently decided here.
func Reconcile(ctx context.Context, fetch PageFunc, since time.Time) (Ledger, error) {
	type key struct {
		flow, currency string
		outcome        biz.Result
	}
	type acc struct {
		sum      int64
		count    int64
		exponent int8
	}
	agg := map[key]*acc{}
	led := Ledger{}

	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return Ledger{}, err
		}
		page, err := fetch(ctx, since, cursor)
		if err != nil {
			return Ledger{}, fmt.Errorf("stripe: reconcile page (after %q): %w", cursor, err)
		}
		for _, pi := range page.Intents {
			if pi == nil {
				continue
			}
			led.Scanned++
			outcome, ok := classify(pi)
			if !ok {
				led.Skipped++
				continue
			}
			currency := strings.ToUpper(string(pi.Currency))
			k := key{flow: pi.Metadata[MetaFlow], currency: currency, outcome: outcome}
			a := agg[k]
			if a == nil {
				a = &acc{exponent: currencyExponent(currency)}
				agg[k] = a
			}
			a.sum += pi.Amount // intended amount — see the Reconcile doc on the basis choice
			a.count++
		}
		// Stop on the provider's signal, or defensively if a page claims more
		// but cannot advance the cursor (empty page, or an id we already used).
		if !page.HasMore || len(page.Intents) == 0 {
			break
		}
		next := page.Intents[len(page.Intents)-1].ID
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}

	led.Rows = make([]biz.LedgerRow, 0, len(agg))
	for k, a := range agg {
		led.Rows = append(led.Rows, biz.LedgerRow{
			Flow:    k.flow,
			Outcome: k.outcome,
			Money:   biz.Money{Amount: a.sum, Currency: k.currency, Exponent: a.exponent},
			Count:   a.count,
		})
	}
	sort.Slice(led.Rows, func(i, j int) bool {
		a, b := led.Rows[i], led.Rows[j]
		if a.Flow != b.Flow {
			return a.Flow < b.Flow
		}
		if a.Money.Currency != b.Money.Currency {
			return a.Money.Currency < b.Money.Currency
		}
		return a.Outcome < b.Outcome
	})
	return led, nil
}

// classify maps a payment intent's terminal status to a ledger outcome. ok is
// false for an intent still in flight or deliberately canceled without a
// payment error — those are not terminal facts and must not be invented into
// the ledger. The reconciled amount is always the intent's `amount` (see the
// Reconcile doc), so classify returns only the outcome.
func classify(pi *stripe.PaymentIntent) (biz.Result, bool) {
	switch pi.Status {
	case stripe.PaymentIntentStatusSucceeded:
		return biz.ResultSuccess, true
	case stripe.PaymentIntentStatusProcessing:
		return biz.ResultDeferred, true
	case stripe.PaymentIntentStatusCanceled,
		stripe.PaymentIntentStatusRequiresPaymentMethod,
		stripe.PaymentIntentStatusRequiresConfirmation,
		stripe.PaymentIntentStatusRequiresAction:
		if pi.LastPaymentError != nil {
			return biz.ResultFailed, true
		}
		return "", false // in flight or a bare cancel — no terminal loss
	default:
		// requires_capture (authorized, awaiting capture) and any status not
		// listed are in flight; skip rather than guess.
		return "", false
	}
}

// ListPageFunc binds Reconcile to the Stripe SDK: it pages payment intents
// through the given payment-intent client, one page per call so Reconcile owns
// the cursor. Wire the client's backend with WrapBackend so each list call is
// observed for biz_provider_calls_total. limit is the page size (Stripe caps it
// at 100; 0 uses the SDK default).
func ListPageFunc(client *paymentintent.Client, limit int64) PageFunc {
	return func(ctx context.Context, since time.Time, startingAfter string) (PaymentIntentPage, error) {
		params := &stripe.PaymentIntentListParams{}
		params.Context = ctx
		params.Single = true // one page; Reconcile drives pagination
		params.CreatedRange = &stripe.RangeQueryParams{GreaterThanOrEqual: since.Unix()}
		if limit > 0 {
			params.Limit = stripe.Int64(limit)
		}
		if startingAfter != "" {
			params.StartingAfter = stripe.String(startingAfter)
		}
		iter := client.List(params)
		page := PaymentIntentPage{}
		for iter.Next() {
			page.Intents = append(page.Intents, iter.PaymentIntent())
		}
		if err := iter.Err(); err != nil {
			return PaymentIntentPage{}, err
		}
		page.HasMore = iter.PaymentIntentList().HasMore
		return page, nil
	}
}
