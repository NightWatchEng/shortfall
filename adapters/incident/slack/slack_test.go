// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/engine/report"
	"github.com/NightWatchEng/shortfall/query"
)

func sampleReport() engine.Report {
	start := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	return engine.Report{
		Request:         engine.Request{Window: query.TimeRange{From: start, To: start.Add(time.Hour)}, Flows: []string{"invoice.pay"}},
		GeneratedAt:     start.Add(2 * time.Hour),
		RegistryVersion: 1,
		LibraryVersion:  "v0.1.0",
		Realized:        engine.Leg{ByCurrency: map[string]int64{"USD": 914900}, Evidence: engine.EvidenceDeterministic},
		Deferred:        engine.DeferredLeg{Leg: engine.Leg{ByCurrency: map[string]int64{}, Evidence: engine.EvidenceDeterministic}},
		Unrealized:      engine.EstLeg{Evidence: engine.EvidenceEstimate, LowMinor: map[string]int64{}, MidMinor: map[string]int64{}, HighMinor: map[string]int64{}},
		Customers:       engine.CustomersLeg{Distinct: 2},
		Coverage:        engine.CoverageLeg{Evidence: engine.EvidenceTrust, Unavailable: "no reconciliation"},
		Severity:        "SEV2",
	}
}

// capture records the last request body a fake Slack received.
type capture struct {
	mu     sync.Mutex
	method string
	auth   string
	body   map[string]string
	calls  int
}

func fakeSlack(t *testing.T, cap *capture, respond func(method string) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(b, &body)
		cap.mu.Lock()
		cap.method = strings.TrimPrefix(r.URL.Path, "/")
		cap.auth = r.Header.Get("Authorization")
		cap.body = body
		cap.calls++
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respond(strings.TrimPrefix(r.URL.Path, "/")))
	}))
}

func TestPostSendsFencedGoldenRender(t *testing.T) {
	cap := &capture{}
	srv := fakeSlack(t, cap, func(string) string { return `{"ok":true,"ts":"1724767200.000100"}` })
	defer srv.Close()
	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	rep := sampleReport()
	ts, err := c.Post(context.Background(), "C123", rep)
	if err != nil {
		t.Fatal(err)
	}
	if ts != "1724767200.000100" {
		t.Fatalf("ts = %q", ts)
	}
	if cap.method != "chat.postMessage" || cap.auth != "Bearer xoxb-test" || cap.body["channel"] != "C123" {
		t.Fatalf("request = %s auth=%q body=%v", cap.method, cap.auth, cap.body)
	}
	// The message is the text ledger block, fenced — matching the golden render.
	want := "```\n" + report.RenderText(rep) + "\n```"
	if cap.body["text"] != want {
		t.Fatalf("posted text does not match the fenced golden render:\n got: %q\nwant: %q", cap.body["text"], want)
	}
}

func TestUpdateEditsSameMessage(t *testing.T) {
	cap := &capture{}
	srv := fakeSlack(t, cap, func(string) string { return `{"ok":true}` })
	defer srv.Close()
	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	if err := c.Update(context.Background(), "C123", "1724767200.000100", sampleReport()); err != nil {
		t.Fatal(err)
	}
	if cap.method != "chat.update" || cap.body["ts"] != "1724767200.000100" {
		t.Fatalf("update request = %s body=%v", cap.method, cap.body)
	}
	if !strings.Contains(cap.body["text"], "REALIZED") {
		t.Fatalf("update text missing the ledger block: %q", cap.body["text"])
	}
}

func TestSlackAPIErrorSurfaced(t *testing.T) {
	cap := &capture{}
	srv := fakeSlack(t, cap, func(string) string { return `{"ok":false,"error":"channel_not_found"}` })
	defer srv.Close()
	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.Post(context.Background(), "C123", sampleReport())
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("Slack ok:false must surface the error, got %v", err)
	}
}

func TestHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	// Pin the non-200 branch specifically: the error must name the HTTP status,
	// not merely be non-nil (an empty body would also fail JSON decode).
	_, err := c.Post(context.Background(), "C123", sampleReport())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("HTTP 500 must surface a status error, got %v", err)
	}
}

func TestRefreshPostsThenUpdatesUntilClosed(t *testing.T) {
	cap := &capture{}
	srv := fakeSlack(t, cap, func(method string) string {
		if method == "chat.postMessage" {
			return `{"ok":true,"ts":"1.1"}`
		}
		return `{"ok":true}`
	})
	defer srv.Close()
	c := New("xoxb-test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	// fetch stays open for two refreshes, then closes.
	var fetches int
	fetch := func(context.Context) (engine.Report, bool, error) {
		fetches++
		return sampleReport(), fetches < 3, nil // open on 1,2; closed on 3
	}
	if err := c.Refresh(context.Background(), "C123", sampleReport(), time.Millisecond, fetch); err != nil {
		t.Fatal(err)
	}
	// 1 post + 2 updates = 3 Slack calls; the 3rd fetch (closed) does not update.
	cap.mu.Lock()
	calls := cap.calls
	cap.mu.Unlock()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (1 post + 2 updates before close)", calls)
	}
}

func TestRefreshRejectsNonPositiveInterval(t *testing.T) {
	c := New("xoxb-test", WithBaseURL("http://unused"))
	err := c.Refresh(context.Background(), "C123", sampleReport(), 0, func(context.Context) (engine.Report, bool, error) {
		return engine.Report{}, false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "interval") {
		t.Fatalf("non-positive interval must error, got %v", err)
	}
}
