// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package pagerduty

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
	path, auth, from, ct string
	body                 map[string]any
}

func newCaptureServer(c *capture, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c.path = req.Method + " " + req.URL.Path
		c.auth = req.Header.Get("Authorization")
		c.from = req.Header.Get("From")
		c.ct = req.Header.Get("Content-Type")
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &c.body)
		w.WriteHeader(status)
	}))
}

// TestWriteImpactPayload pins the custom-fields write: method, path, Token
// auth, and the by-name field mapping carrying the impact line.
func TestWriteImpactPayload(t *testing.T) {
	var got capture
	srv := newCaptureServer(&got, 200)
	defer srv.Close()
	c := New("pd_key", "Dollar impact", "oncall@example.com", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "PXYZ", fixtureReport()); err != nil {
		t.Fatal(err)
	}

	if got.path != "PUT /incidents/PXYZ/custom_fields/values" {
		t.Fatalf("path = %q", got.path)
	}

	if got.auth != "Token token=pd_key" || got.ct != "application/json" {
		t.Fatalf("headers = %q / %q", got.auth, got.ct)
	}

	if got.from != "" {
		t.Fatalf("custom-fields write must not send From, got %q", got.from)
	}

	f := got.body["custom_fields"].([]any)[0].(map[string]any)
	if f["name"] != "Dollar impact" || !strings.Contains(f["value"].(string), "realized [deterministic] USD 16999") {
		t.Fatalf("custom_fields = %v", f)
	}
}

// TestAttachCustomersCSV pins the note write: the CSV rides note.content and
// the required From header carries the requester email; an unavailable
// customers leg propagates its reason instead of posting.
func TestAttachCustomersCSV(t *testing.T) {
	var got capture
	srv := newCaptureServer(&got, 201)
	defer srv.Close()
	c := New("pd_key", "Dollar impact", "oncall@example.com", WithBaseURL(srv.URL))
	if err := c.AttachCustomersCSV(context.Background(), "PXYZ", fixtureReport()); err != nil {
		t.Fatal(err)
	}

	if got.path != "POST /incidents/PXYZ/notes" {
		t.Fatalf("path = %q", got.path)
	}

	if got.from != "oncall@example.com" {
		t.Fatalf("From = %q", got.from)
	}

	content := got.body["note"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "h:c000001,smb,USD,14900") {
		t.Fatalf("note content = %q", content)
	}

	r := fixtureReport()
	r.Customers = engine.CustomersLeg{NotAvailableReason: "backend serves no events"}
	if err := c.AttachCustomersCSV(context.Background(), "PXYZ", r); err == nil || !strings.Contains(err.Error(), "no events") {
		t.Fatalf("want unavailability error, got %v", err)
	}
}

// TestWriteImpactErrorStatus pins fail-loud on a non-2xx write.
func TestWriteImpactErrorStatus(t *testing.T) {
	var got capture
	srv := newCaptureServer(&got, 403)
	defer srv.Close()
	c := New("k", "f", "e@example.com", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "P1", fixtureReport()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want status error, got %v", err)
	}
}
