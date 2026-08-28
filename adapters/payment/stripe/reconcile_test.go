package stripe

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/form"
	"github.com/stripe/stripe-go/v79/paymentintent"

	"github.com/NightWatchEng/shortfall/biz"
)

// pi builds a payment-intent fixture. received is the captured amount, set only
// to model a partial capture (received < amount) — the reconciler deliberately
// ignores it and reconciles on the intended amount, which these fixtures verify.
// withErr attaches a last_payment_error so a non-succeeded status maps to a
// failed loss.
func pi(id string, status stripe.PaymentIntentStatus, amount, received int64, currency, flow string, withErr bool) *stripe.PaymentIntent {
	p := &stripe.PaymentIntent{
		ID: id, Status: status, Amount: amount, AmountReceived: received,
		Currency: stripe.Currency(currency),
	}
	if flow != "" {
		p.Metadata = map[string]string{MetaFlow: flow}
	}
	if withErr {
		p.LastPaymentError = &stripe.Error{Code: stripe.ErrorCodeCardDeclined}
	}
	return p
}

// pagedFetch returns a PageFunc that serves intents in fixed-size pages and
// records the cursors it was asked for, so a test can assert the walk followed
// starting_after correctly.
func pagedFetch(all []*stripe.PaymentIntent, size int, cursors *[]string) PageFunc {
	return func(_ context.Context, _ time.Time, startingAfter string) (PaymentIntentPage, error) {
		*cursors = append(*cursors, startingAfter)
		start := 0
		if startingAfter != "" {
			for i, p := range all {
				if p.ID == startingAfter {
					start = i + 1
					break
				}
			}
		}
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		return PaymentIntentPage{Intents: all[start:end], HasMore: end < len(all)}, nil
	}
}

func rowFor(rows []biz.LedgerRow, flow, currency string, outcome biz.Result) (biz.LedgerRow, bool) {
	for _, r := range rows {
		if r.Flow == flow && r.Money.Currency == currency && r.Outcome == outcome {
			return r, true
		}
	}
	return biz.LedgerRow{}, false
}

func TestReconcileAggregatesAndReconciles100(t *testing.T) {
	// A mixed fixture set: successes (captured), a partial capture, a decline
	// (failed), a processing (deferred), across two flows and two currencies,
	// plus intents that must NOT become rows (bare cancel, requires_capture).
	all := []*stripe.PaymentIntent{
		pi("pi_1", stripe.PaymentIntentStatusSucceeded, 10000, 10000, "usd", "checkout.pay", false),
		pi("pi_2", stripe.PaymentIntentStatusSucceeded, 5000, 4000, "usd", "checkout.pay", false), // partial capture: received < amount
		pi("pi_3", stripe.PaymentIntentStatusRequiresPaymentMethod, 7000, 0, "usd", "checkout.pay", true),
		pi("pi_4", stripe.PaymentIntentStatusProcessing, 3000, 0, "usd", "checkout.pay", false),
		pi("pi_5", stripe.PaymentIntentStatusSucceeded, 20000, 20000, "eur", "invoice.pay", false),
		pi("pi_6", stripe.PaymentIntentStatusCanceled, 9000, 0, "eur", "invoice.pay", true),         // canceled after a payment error -> failed
		pi("pi_7", stripe.PaymentIntentStatusCanceled, 1000, 0, "eur", "invoice.pay", false),        // deliberate cancel -> skipped
		pi("pi_8", stripe.PaymentIntentStatusRequiresCapture, 2000, 0, "eur", "invoice.pay", false), // authorized, in flight -> skipped
	}

	var cursors []string
	led, err := Reconcile(context.Background(), pagedFetch(all, 3, &cursors), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}

	if led.Scanned != 8 {
		t.Fatalf("scanned = %d, want 8", led.Scanned)
	}
	if led.Skipped != 2 { // pi_7 (bare cancel) + pi_8 (requires_capture)
		t.Fatalf("skipped = %d, want 2", led.Skipped)
	}

	// Independent reconciliation oracle: the expected rows are computed by hand
	// on the TELEMETRY basis — the intended `amount`, the same field the webhook
	// path records for a succeeded event — NOT the reconciler's own output and
	// NOT amount_received. pi_2 is a partial capture (amount 5000, received
	// 4000): the oracle counts 5000, so if the reconciler ever reverts to
	// amount_received the checkout success row (15000 vs 14000) makes this fail.
	// This is what makes "reconciles to 100%" a real cross-check rather than a
	// tautology over the reconciler's own basis.
	type want struct {
		flow, currency string
		outcome        biz.Result
		sum, count     int64
	}
	expected := []want{
		{"checkout.pay", "USD", biz.ResultSuccess, 10000 + 5000, 2}, // amount, not amount_received
		{"checkout.pay", "USD", biz.ResultFailed, 7000, 1},
		{"checkout.pay", "USD", biz.ResultDeferred, 3000, 1},
		{"invoice.pay", "EUR", biz.ResultSuccess, 20000, 1},
		{"invoice.pay", "EUR", biz.ResultFailed, 9000, 1},
	}
	if len(led.Rows) != len(expected) {
		t.Fatalf("rows = %d, want %d distinct (flow,currency,outcome) slices: %+v", len(led.Rows), len(expected), led.Rows)
	}
	for _, w := range expected {
		r, ok := rowFor(led.Rows, w.flow, w.currency, w.outcome)
		if !ok {
			t.Fatalf("missing row %s/%s/%s", w.flow, w.currency, w.outcome)
		}
		if r.Money.Amount != w.sum || r.Count != w.count {
			t.Fatalf("row %s/%s/%s = %d/%d, want %d/%d", w.flow, w.currency, w.outcome, r.Money.Amount, r.Count, w.sum, w.count)
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("row %+v invalid: %v", r, err)
		}
	}
	// Deterministic order: checkout.pay before invoice.pay.
	if led.Rows[0].Flow != "checkout.pay" || led.Rows[len(led.Rows)-1].Flow != "invoice.pay" {
		t.Fatalf("rows not ordered by flow: %+v", led.Rows)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		status  stripe.PaymentIntentStatus
		withErr bool
		want    biz.Result
		ok      bool
	}{
		{"succeeded -> success", stripe.PaymentIntentStatusSucceeded, false, biz.ResultSuccess, true},
		{"processing -> deferred", stripe.PaymentIntentStatusProcessing, false, biz.ResultDeferred, true},
		{"canceled + error -> failed", stripe.PaymentIntentStatusCanceled, true, biz.ResultFailed, true},
		{"canceled, no error -> skip", stripe.PaymentIntentStatusCanceled, false, "", false},
		{"requires_payment_method + error -> failed", stripe.PaymentIntentStatusRequiresPaymentMethod, true, biz.ResultFailed, true},
		{"requires_payment_method, no error -> skip", stripe.PaymentIntentStatusRequiresPaymentMethod, false, "", false},
		{"requires_confirmation + error -> failed", stripe.PaymentIntentStatusRequiresConfirmation, true, biz.ResultFailed, true},
		{"requires_confirmation, no error -> skip", stripe.PaymentIntentStatusRequiresConfirmation, false, "", false},
		{"requires_action + error -> failed", stripe.PaymentIntentStatusRequiresAction, true, biz.ResultFailed, true},
		{"requires_action, no error -> skip", stripe.PaymentIntentStatusRequiresAction, false, "", false},
		{"requires_capture -> skip (in flight)", stripe.PaymentIntentStatusRequiresCapture, false, "", false},
		{"requires_capture with error -> still skip (not terminal)", stripe.PaymentIntentStatusRequiresCapture, true, "", false},
		{"unknown/future status -> skip", stripe.PaymentIntentStatus("some_future_status"), false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classify(pi("pi_x", c.status, 1000, 1000, "usd", "f", c.withErr))
			if got != c.want || ok != c.ok {
				t.Fatalf("classify(%s, err=%v) = %q/%v, want %q/%v", c.status, c.withErr, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestReconcilePaginationBoundaries(t *testing.T) {
	// 7 succeeded intents, page size 3 -> pages of 3,3,1. Assert every page is
	// walked and the cursor follows the last id of each page (the boundary).
	var all []*stripe.PaymentIntent
	for i, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		_ = i
		all = append(all, pi("pi_"+id, stripe.PaymentIntentStatusSucceeded, 1000, 1000, "usd", "f", false))
	}
	var cursors []string
	led, err := Reconcile(context.Background(), pagedFetch(all, 3, &cursors), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if led.Scanned != 7 {
		t.Fatalf("scanned = %d, want 7", led.Scanned)
	}
	if r, _ := rowFor(led.Rows, "f", "USD", biz.ResultSuccess); r.Count != 7 || r.Money.Amount != 7000 {
		t.Fatalf("aggregated = %+v, want 7000/7", r)
	}
	// Cursors: "" (page 1), "pi_c" (after page 1), "pi_f" (after page 2). Page 3
	// returns 1 item with HasMore=false, so the walk stops — no 4th fetch.
	want := []string{"", "pi_c", "pi_f"}
	if len(cursors) != len(want) {
		t.Fatalf("cursors = %v, want %v", cursors, want)
	}
	for i := range want {
		if cursors[i] != want[i] {
			t.Fatalf("cursor[%d] = %q, want %q", i, cursors[i], want[i])
		}
	}
}

func TestReconcileExactPageBoundaryHasMore(t *testing.T) {
	// A provider may return a full final page with HasMore=true, then an empty
	// page. Reconcile must consume the empty page (to learn HasMore=false) and
	// not double count or spin.
	all := []*stripe.PaymentIntent{
		pi("pi_1", stripe.PaymentIntentStatusSucceeded, 1000, 1000, "usd", "f", false),
		pi("pi_2", stripe.PaymentIntentStatusSucceeded, 1000, 1000, "usd", "f", false),
	}
	calls := 0
	fetch := func(_ context.Context, _ time.Time, after string) (PaymentIntentPage, error) {
		calls++
		switch after {
		case "":
			// full page, but claim more (exact-boundary case)
			return PaymentIntentPage{Intents: all, HasMore: true}, nil
		default:
			// empty follow-up page: defensively ends the walk
			return PaymentIntentPage{Intents: nil, HasMore: false}, nil
		}
	}
	led, err := Reconcile(context.Background(), fetch, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if led.Scanned != 2 || calls != 2 {
		t.Fatalf("scanned=%d calls=%d, want 2 and 2", led.Scanned, calls)
	}
	if r, _ := rowFor(led.Rows, "f", "USD", biz.ResultSuccess); r.Count != 2 {
		t.Fatalf("count = %d, want 2 (no double count)", r.Count)
	}
}

func TestReconcileStopsOnNonAdvancingCursor(t *testing.T) {
	// A misbehaving pager that always returns the same page with HasMore=true
	// must not loop forever — the cursor cannot advance, so the walk stops.
	same := []*stripe.PaymentIntent{pi("pi_x", stripe.PaymentIntentStatusSucceeded, 1000, 1000, "usd", "f", false)}
	calls := 0
	fetch := func(_ context.Context, _ time.Time, _ string) (PaymentIntentPage, error) {
		calls++
		if calls > 10 {
			t.Fatal("Reconcile looped on a non-advancing cursor")
		}
		return PaymentIntentPage{Intents: same, HasMore: true}, nil
	}
	led, err := Reconcile(context.Background(), fetch, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	// First page consumed, then cursor "pi_x" equals the next page's last id -> stop.
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (stop once cursor cannot advance)", calls)
	}
	if led.Scanned != 2 { // pi_x seen on both fetches; that is the documented cost of the guard
		t.Fatalf("scanned = %d", led.Scanned)
	}
}

func TestReconcilePropagatesFetchError(t *testing.T) {
	boom := errors.New("network down")
	fetch := func(_ context.Context, _ time.Time, _ string) (PaymentIntentPage, error) {
		return PaymentIntentPage{}, boom
	}
	_, err := Reconcile(context.Background(), fetch, time.Unix(0, 0))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
}

func TestReconcileHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetch := func(_ context.Context, _ time.Time, _ string) (PaymentIntentPage, error) {
		t.Fatal("fetch must not run under a canceled context")
		return PaymentIntentPage{}, nil
	}
	if _, err := Reconcile(ctx, fetch, time.Unix(0, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// listBackend is a stripe.Backend whose CallRaw serves scripted PaymentIntent
// list pages and records the query form of each call, so ListPageFunc's param
// translation (created[gte], limit, starting_after) and one-page-per-call
// behavior can be asserted without a live API.
type listBackend struct {
	page    []*stripe.PaymentIntent
	hasMore bool
	forms   []*form.Values
}

func (b *listBackend) CallRaw(_, _, _ string, body *form.Values, _ *stripe.Params, v stripe.LastResponseSetter) error {
	b.forms = append(b.forms, body)
	if pil, ok := v.(*stripe.PaymentIntentList); ok {
		pil.Data = b.page
		pil.HasMore = b.hasMore
	}
	return nil
}
func (b *listBackend) Call(_, _, _ string, _ stripe.ParamsContainer, _ stripe.LastResponseSetter) error {
	return nil
}
func (b *listBackend) CallStreaming(_, _, _ string, _ stripe.ParamsContainer, _ stripe.StreamingLastResponseSetter) error {
	return nil
}
func (b *listBackend) CallMultipart(_, _, _, _ string, _ *bytes.Buffer, _ *stripe.Params, _ stripe.LastResponseSetter) error {
	return nil
}
func (b *listBackend) SetMaxNetworkRetries(int64) {}

func TestListPageFuncTranslatesParamsAndPages(t *testing.T) {
	be := &listBackend{
		page:    []*stripe.PaymentIntent{pi("pi_a", stripe.PaymentIntentStatusSucceeded, 1000, 1000, "usd", "f", false)},
		hasMore: true,
	}
	client := &paymentintent.Client{B: be, Key: "sk_test_x"}
	fn := ListPageFunc(client, 50)
	since := time.Unix(1_700_000_000, 0)

	// First page: created[gte] set, limit set, no starting_after.
	page, err := fn(context.Background(), since, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Intents) != 1 || page.Intents[0].ID != "pi_a" || !page.HasMore {
		t.Fatalf("page = %+v", page)
	}
	f0 := be.forms[0]
	if got := f0.Get("created[gte]"); len(got) != 1 || got[0] != strconv.FormatInt(since.Unix(), 10) {
		t.Fatalf("created[gte] = %v, want %d", got, since.Unix())
	}
	if got := f0.Get("limit"); len(got) != 1 || got[0] != "50" {
		t.Fatalf("limit = %v, want 50", got)
	}
	if got := f0.Get("starting_after"); len(got) != 0 {
		t.Fatalf("starting_after should be unset on the first page, got %v", got)
	}

	// Second page: starting_after threaded through.
	if _, err := fn(context.Background(), since, "pi_a"); err != nil {
		t.Fatal(err)
	}
	if got := be.forms[1].Get("starting_after"); len(got) != 1 || got[0] != "pi_a" {
		t.Fatalf("starting_after = %v, want pi_a", got)
	}
}

func TestReconcileUnattributedFlow(t *testing.T) {
	// An intent with no biz_flow metadata still reconciles — under flow "" — so
	// the provider total is complete; the caller/coverage leg decides how to
	// treat unattributed money rather than dropping it.
	all := []*stripe.PaymentIntent{
		pi("pi_1", stripe.PaymentIntentStatusSucceeded, 5000, 5000, "usd", "", false),
	}
	var cursors []string
	led, err := Reconcile(context.Background(), pagedFetch(all, 10, &cursors), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := rowFor(led.Rows, "", "USD", biz.ResultSuccess); !ok || r.Money.Amount != 5000 {
		t.Fatalf("unattributed row = %+v (ok=%v), want 5000", r, ok)
	}
}
