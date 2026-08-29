package rootly

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

// TestWriteImpactPayload pins the JSON:API update's method, path, auth,
// content type, and field mapping: the summary attribute carries the
// impact line inside the data/type/attributes envelope.
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

	c := New("rootly_key", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "inc-42", fixtureReport()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "PUT /v1/incidents/inc-42" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer rootly_key" || gotCT != "application/vnd.api+json" {
		t.Fatalf("headers = %q / %q", gotAuth, gotCT)
	}
	data := gotBody["data"].(map[string]any)
	if data["type"] != "incidents" {
		t.Fatalf("data.type = %v", data["type"])
	}
	summary := data["attributes"].(map[string]any)["summary"].(string)
	if !strings.Contains(summary, "realized [deterministic] USD 16999") {
		t.Fatalf("summary = %q", summary)
	}
}

// TestWriteImpactErrorStatus pins fail-loud on a non-2xx write.
func TestWriteImpactErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := New("k", WithBaseURL(srv.URL))
	if err := c.WriteImpact(context.Background(), "inc-1", fixtureReport()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want status error, got %v", err)
	}
}
