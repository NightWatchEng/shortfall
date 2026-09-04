// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package stripe

import (
	"io"
	"net/http"

	"github.com/NightWatchEng/shortfall/biz"
)

// maxWebhookBody caps the request body read. Stripe webhooks are small; a cap
// stops an oversized/hung body from consuming memory before signature
// verification even runs.
const maxWebhookBody = 1 << 20 // 1 MiB

// Handler returns an http.Handler webhook receiver. It reads the body (capped),
// verifies the Stripe-Signature over it, and — for a mapped event — delivers
// the outcome to sink. Responses: 400 on a read or signature failure (Stripe
// retries), 200 otherwise (mapped delivered, or a verified event this adapter
// ignores). Options such as WithStageMap apply to every mapped event. sink
// must not block; wire it to a non-blocking emitter. The outcome already
// carries its ValueContext, so the sink stamps a fresh context from it —
// there is no inbound request context worth inheriting:
//
//	stripe.Handler(secret, func(o biz.Outcome) {
//	    ctx, err := biz.WithValueContext(context.Background(), o.VC)
//	    if err != nil {
//	        return // rejected at the biz boundary; Record would drop it anyway
//	    }
//
//	    em.Record(ctx, o.Stage, o.Result, emit.WithSource(o.Source), emit.WithAt(o.At))
//	})
func Handler(secret string, sink func(biz.Outcome), opts ...Option) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}

		out, mapped, err := VerifyAndMap(body, r.Header.Get("Stripe-Signature"), secret, opts...)
		if err != nil {
			// Reject unverified payloads — a forged event must never be
			// delivered as an outcome.
			http.Error(w, "signature verification failed", http.StatusBadRequest)
			return
		}

		if mapped && sink != nil {
			sink(out)
		}

		w.WriteHeader(http.StatusOK)
	})
}
