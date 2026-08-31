// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package firehydrant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/query"
)

func fixtureReport() engine.Report {
	from := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	return engine.Report{
		Request: engine.Request{
			Window: query.TimeRange{From: from, To: from.Add(time.Hour)},
			Flows:  []string{"invoice.pay"},
		},
		Realized: engine.Leg{ByCurrency: map[string]int64{"USD": 16999}, Evidence: engine.EvidenceDeterministic},
		Customers: engine.CustomersLeg{
			Distinct: 1,
			TopN: []engine.CustomerImpact{
				{CustomerID: "h:c000001", Segment: "smb", ByCurrency: map[string]int64{"USD": 14900}},
			},
		},
		Severity: "SEV2",
	}
}

type capture struct {
	path, auth, ct string
	body           map[string]any
}

func newCaptureServer(c *capture, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c.path = req.Method + " " + req.URL.Path
		c.auth = req.Header.Get("Authorization")
		c.ct = req.Header.Get("Content-Type")
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &c.body)
		w.WriteHeader(status)
	}))
}

// TestWriteImpact pins the two field mappings: the native
// customer_impact_summary by default, a custom field's value_string when
// configured — one PATCH either way.
func TestWriteImpact(t *testing.T) {
	cases := []struct {
		name  string
		opts  []Option
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "default writes customer_impact_summary",
			check: func(t *testing.T, body map[string]any) {
				s, _ := body["customer_impact_summary"].(string)
				if !strings.Contains(s, "realized [deterministic] USD 16999") {
					t.Fatalf("customer_impact_summary = %q", s)
				}
			},
		},
		{
			name: "custom field writes value_string",
			opts: []Option{WithCustomFieldID("fld-1")},
			check: func(t *testing.T, body map[string]any) {
				f := body["custom_fields"].([]any)[0].(map[string]any)
				if f["field_id"] != "fld-1" || !strings.Contains(f["value_string"].(string), "USD 16999") {
					t.Fatalf("custom_fields = %v", f)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got capture
			srv := newCaptureServer(&got, 200)
			defer srv.Close()
			c := New("fhb-token", append(tc.opts, WithBaseURL(srv.URL))...)
			if err := c.WriteImpact(context.Background(), "inc-7", fixtureReport()); err != nil {
				t.Fatal(err)
			}
			if got.path != "PATCH /v1/incidents/inc-7" {
				t.Fatalf("path = %q", got.path)
			}
			if got.auth != "Bearer fhb-token" || got.ct != "application/json" {
				t.Fatalf("headers = %q / %q", got.auth, got.ct)
			}
			tc.check(t, got.body)
		})
	}
}

// TestAttachCustomersCSV pins the note payload: the CSV rides the note body,
// and an unavailable customers leg propagates its reason instead of posting.
func TestAttachCustomersCSV(t *testing.T) {
	var got capture
	srv := newCaptureServer(&got, 201)
	defer srv.Close()
	c := New("fhb-token", WithBaseURL(srv.URL))
	if err := c.AttachCustomersCSV(context.Background(), "inc-7", fixtureReport()); err != nil {
		t.Fatal(err)
	}
	if got.path != "POST /v1/incidents/inc-7/notes" {
		t.Fatalf("path = %q", got.path)
	}
	body, _ := got.body["body"].(string)
	if !strings.Contains(body, "customer_id,segment,currency,amount_minor") ||
		!strings.Contains(body, "h:c000001,smb,USD,14900") {
		t.Fatalf("note body = %q", body)
	}

	r := fixtureReport()
	r.Customers = engine.CustomersLeg{NotAvailableReason: "backend serves no events"}
	if err := c.AttachCustomersCSV(context.Background(), "inc-7", r); err == nil || !strings.Contains(err.Error(), "no events") {
		t.Fatalf("want unavailability error, got %v", err)
	}
}

// TestWriteImpactErrorStatus pins fail-loud on a non-2xx write.
func TestWriteImpactErrorStatus(t *testing.T) {
	var got capture
	srv := newCaptureServer(&got, 500)
	defer srv.Close()
	c := New("k", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "inc-1", fixtureReport()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status error, got %v", err)
	}
}
