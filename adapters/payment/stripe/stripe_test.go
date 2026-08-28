package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v79"

	"github.com/NightWatchEng/shortfall/biz"
)

const testSecret = "whsec_testsecret"

// sign builds a valid Stripe-Signature header for payload (the scheme Stripe
// uses: t=<ts>,v1=<hex hmac_sha256(secret, "<ts>.<payload>")>).
func sign(payload []byte, secret string, ts time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", ts.Unix(), payload)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// fixture builds a signed webhook payload for one event type + object body.
func fixture(t *testing.T, eventType string, created time.Time, objectJSON string) ([]byte, string) {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"id":"evt_1","type":%q,"created":%d,"data":{"object":%s}}`,
		eventType, created.Unix(), objectJSON))
	return payload, sign(payload, testSecret, time.Now())
}

const meta = `"metadata":{"biz_flow":"invoice.pay","biz_entity":"inv_1","biz_customer":"h:c1"}`

func TestVerifyAndMapEventTypes(t *testing.T) {
	created := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		eventType  string
		objectJSON string
		wantStage  string
		wantResult biz.Result
		wantAmount int64
		wantCur    string
		wantExp    int8
	}{
		{"pi succeeded", "payment_intent.succeeded", `{"amount":14900,"currency":"usd",` + meta + `}`, "capture", biz.ResultSuccess, 14900, "USD", 2},
		{"pi failed", "payment_intent.payment_failed", `{"amount":14900,"currency":"usd",` + meta + `}`, "capture", biz.ResultFailed, 14900, "USD", 2},
		{"pi processing -> deferred", "payment_intent.processing", `{"amount":5000,"currency":"usd",` + meta + `}`, "capture", biz.ResultDeferred, 5000, "USD", 2},
		{"pi requires_action -> auth deferred", "payment_intent.requires_action", `{"amount":5000,"currency":"usd",` + meta + `}`, "auth", biz.ResultDeferred, 5000, "USD", 2},
		{"charge failed", "charge.failed", `{"amount":700,"currency":"eur",` + meta + `}`, "capture", biz.ResultFailed, 700, "EUR", 2},
		{"invoice paid uses amount_paid", "invoice.paid", `{"amount_paid":9900,"amount_due":9900,"currency":"usd",` + meta + `}`, "settle", biz.ResultSuccess, 9900, "USD", 2},
		{"invoice failed uses amount_due", "invoice.payment_failed", `{"amount_paid":0,"amount_due":9900,"currency":"usd",` + meta + `}`, "settle", biz.ResultFailed, 9900, "USD", 2},
		{"dispute created", "charge.dispute.created", `{"amount":14900,"currency":"usd",` + meta + `}`, "dispute", biz.ResultFailed, 14900, "USD", 2},
		{"zero-decimal currency exponent", "payment_intent.succeeded", `{"amount":5000,"currency":"jpy",` + meta + `}`, "capture", biz.ResultSuccess, 5000, "JPY", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload, sig := fixture(t, c.eventType, created, c.objectJSON)
			out, mapped, err := VerifyAndMap(payload, sig, testSecret)
			if err != nil {
				t.Fatal(err)
			}
			if !mapped {
				t.Fatal("event should be mapped")
			}
			if out.Stage != c.wantStage || out.Result != c.wantResult {
				t.Fatalf("stage/result = %q/%q, want %q/%q", out.Stage, out.Result, c.wantStage, c.wantResult)
			}
			if out.VC.Money.Amount != c.wantAmount || out.VC.Money.Currency != c.wantCur || out.VC.Money.Exponent != c.wantExp {
				t.Fatalf("money = %+v, want %d %s exp %d", out.VC.Money, c.wantAmount, c.wantCur, c.wantExp)
			}
			if out.VC.Flow != "invoice.pay" || out.VC.EntityID != "inv_1" || out.VC.CustomerID != "h:c1" {
				t.Fatalf("VC metadata not read: %+v", out.VC)
			}
			if !out.At.Equal(created) {
				t.Fatalf("At = %v, want the event's created time %v", out.At, created)
			}
			if out.Source != Source {
				t.Fatalf("source = %q", out.Source)
			}
		})
	}
}

func TestSignatureFailureIsRejected(t *testing.T) {
	created := time.Now()
	payload, goodSig := fixture(t, "payment_intent.payment_failed", created, `{"amount":99999,"currency":"usd",`+meta+`}`)
	cases := []struct {
		name string
		sig  string
	}{
		{"wrong secret", sign(payload, "whsec_attacker", time.Now())},
		{"tampered payload", goodSig}, // valid sig, but we mutate the payload below
		{"empty signature", ""},
		{"garbage signature", "t=123,v1=deadbeef"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := payload
			if c.name == "tampered payload" {
				p = []byte(strings.Replace(string(payload), "99999", "1", 1)) // amount changed after signing
			}
			_, mapped, err := VerifyAndMap(p, c.sig, testSecret)
			if err == nil {
				t.Fatal("an invalid signature MUST be rejected — a forged event cannot invent a loss")
			}
			if mapped {
				t.Fatal("a rejected payload must never be mapped")
			}
		})
	}
}

func TestUnmappedEventIgnored(t *testing.T) {
	payload, sig := fixture(t, "customer.created", time.Now(), `{"id":"cus_1"}`)
	out, mapped, err := VerifyAndMap(payload, sig, testSecret)
	if err != nil {
		t.Fatalf("a verified unmapped event must not error: %v", err)
	}
	if mapped {
		t.Fatalf("customer.created should not map, got %+v", out)
	}
}

type fakeParams struct{ md map[string]string }

func (f *fakeParams) AddMetadata(k, v string) {
	if f.md == nil {
		f.md = map[string]string{}
	}
	f.md[k] = v
}

func TestWithStripeMetadataStamps(t *testing.T) {
	p := &fakeParams{}
	WithStripeMetadata(p, biz.ValueContext{Flow: "invoice.pay", EntityID: "inv_9", CustomerID: "h:c9"})
	if p.md[MetaFlow] != "invoice.pay" || p.md[MetaEntity] != "inv_9" || p.md[MetaCustomer] != "h:c9" {
		t.Fatalf("metadata = %v", p.md)
	}
	// The real *stripe.Params and resource params satisfy MetadataSetter.
	var _ MetadataSetter = &stripe.Params{}
}

func TestHandlerVerifiesAndDelivers(t *testing.T) {
	var got []biz.Outcome
	h := Handler(testSecret, func(o biz.Outcome) { got = append(got, o) })

	// Valid signed webhook -> 200, delivered.
	payload, sig := fixture(t, "payment_intent.payment_failed", time.Now(), `{"amount":14900,"currency":"usd",`+meta+`}`)
	rec := postWebhook(t, h, payload, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid webhook: code %d", rec.Code)
	}
	if len(got) != 1 || got[0].VC.Money.Amount != 14900 {
		t.Fatalf("outcome not delivered: %+v", got)
	}

	// Bad signature -> 400, nothing delivered.
	got = nil
	rec = postWebhook(t, h, payload, "t=1,v1=bad")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-signature webhook: code %d, want 400", rec.Code)
	}
	if len(got) != 0 {
		t.Fatal("a bad-signature webhook must deliver nothing")
	}
}

func postWebhook(t *testing.T, h http.Handler, payload []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Stripe-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
