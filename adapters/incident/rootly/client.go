// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package rootly writes a shortfall impact summary into a Rootly incident.
// Rootly is a consumer of the number, not a producer: this is a thin writer
// over the JSON:API incident update — a plain *http.Client, no SDK, base
// URL overridable so tests run against httptest.
//
// The summary lands in the incident's `summary` attribute — Rootly's
// incident update surface has no dedicated impact field — so wire this to
// incidents whose summary is owned by automation, not hand-written.
package rootly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/engine/report"
)

const defaultBaseURL = "https://api.rootly.com"

// Client writes impact summaries to Rootly.
type Client struct {
	http    *http.Client
	token   string
	baseURL string
}

// Option configures New.
type Option func(*Client)

// WithHTTPClient sets the HTTP client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithBaseURL overrides the API base URL (test seam).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// New builds a Client for an API key.
func New(token string, opts ...Option) *Client {
	c := &Client{http: &http.Client{Timeout: 10 * time.Second}, token: token, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}

	return c
}

// WriteImpact sets the incident's summary attribute to the report's impact
// summary via PUT /v1/incidents/{id} (JSON:API envelope).
func (c *Client) WriteImpact(ctx context.Context, incidentID string, r engine.Report) error {
	body := map[string]any{
		"data": map[string]any{
			"type": "incidents",
			"attributes": map[string]any{
				"summary": report.Summary(r),
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/v1/incidents/%s", c.baseURL, neturl.PathEscape(incidentID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/vnd.api+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("rootly: update: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("rootly: update: status %d: %s", resp.StatusCode, snippet)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
