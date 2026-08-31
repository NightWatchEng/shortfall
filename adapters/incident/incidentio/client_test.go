// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package incidentio

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
		Severity: "SEV2",
	}
}

// TestWriteImpactPayload pins the V2 Edit action's method, path, auth, and
// field mapping: the summary lands as value_text on the configured custom
// field, and the edit never notifies the incident channel.
func TestWriteImpactPayload(t *testing.T) {
	var gotPath, gotAuth, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.Method + " " + req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		gotCT = req.Header.Get("Content-Type")
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New("inc_test_key", "01CUSTOMFIELD", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "01INCIDENT", fixtureReport()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /v2/incidents/01INCIDENT/actions/edit" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer inc_test_key" || gotCT != "application/json" {
		t.Fatalf("headers = %q / %q", gotAuth, gotCT)
	}
	if notify, ok := gotBody["notify_incident_channel"].(bool); !ok || notify {
		t.Fatalf("notify_incident_channel = %v, want false", gotBody["notify_incident_channel"])
	}
	entries := gotBody["incident"].(map[string]any)["custom_field_entries"].([]any)
	entry := entries[0].(map[string]any)
	if entry["custom_field_id"] != "01CUSTOMFIELD" {
		t.Fatalf("custom_field_id = %v", entry["custom_field_id"])
	}
	text := entry["values"].([]any)[0].(map[string]any)["value_text"].(string)
	if !strings.Contains(text, "realized [deterministic] USD 16999") || !strings.Contains(text, "suggested SEV2") {
		t.Fatalf("value_text = %q", text)
	}
}

// TestWriteImpactErrorStatus pins fail-loud: a non-2xx write is an error,
// never a silent no-op on the money number.
func TestWriteImpactErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
	}))
	defer srv.Close()
	c := New("k", "f", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "01X", fixtureReport()); err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("want status error, got %v", err)
	}
}
