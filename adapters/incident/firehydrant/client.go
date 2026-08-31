// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package firehydrant writes a shortfall impact summary into a FireHydrant
// incident. FireHydrant is a consumer of the number, not a producer: a thin
// writer over PATCH /v1/incidents — plain *http.Client, no SDK, base URL
// overridable so tests run against httptest.
//
// By default the summary lands in FireHydrant's native
// customer_impact_summary field; WithCustomFieldID targets a string custom
// field instead. AttachCustomersCSV posts the top-accounts CSV as an
// incident note (private to the org).
package firehydrant

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

const defaultBaseURL = "https://api.firehydrant.io"

// Client writes impact summaries to FireHydrant.
type Client struct {
	http          *http.Client
	token         string
	baseURL       string
	customFieldID string
}

// Option configures New.
type Option func(*Client)

// WithHTTPClient sets the HTTP client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithBaseURL overrides the API base URL (test seam).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithCustomFieldID writes the summary into a string custom field instead
// of the native customer_impact_summary.
func WithCustomFieldID(id string) Option { return func(c *Client) { c.customFieldID = id } }

// New builds a Client for a bot token (fhb-...).
func New(token string, opts ...Option) *Client {
	c := &Client{http: &http.Client{Timeout: 10 * time.Second}, token: token, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}

	return c
}

// WriteImpact writes the report's impact summary via PATCH
// /v1/incidents/{id} — into customer_impact_summary, or the configured
// custom field's value_string.
func (c *Client) WriteImpact(ctx context.Context, incidentID string, r engine.Report) error {
	summary := report.Summary(r)
	var body map[string]any
	if c.customFieldID != "" {
		body = map[string]any{
			"custom_fields": []map[string]any{{
				"field_id": c.customFieldID, "value_string": summary,
			}},
		}
	} else {
		body = map[string]any{"customer_impact_summary": summary}
	}

	return c.call(ctx, http.MethodPatch, fmt.Sprintf("/v1/incidents/%s", neturl.PathEscape(incidentID)), body, "update")
}

// AttachCustomersCSV posts the customers leg's top accounts as an incident
// note. A leg that says why it is unavailable propagates that reason
// instead of posting an empty file.
func (c *Client) AttachCustomersCSV(ctx context.Context, incidentID string, r engine.Report) error {
	csv, err := report.CustomersCSV(r)
	if err != nil {
		return fmt.Errorf("firehydrant: %w", err)
	}

	body := map[string]any{
		"body": "shortfall customers (top accounts, minor units):\n```\n" + string(csv) + "```",
	}
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/v1/incidents/%s/notes", neturl.PathEscape(incidentID)), body, "note")
}

func (c *Client) call(ctx context.Context, method, path string, body map[string]any, op string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firehydrant: %s: %w", op, err)
	}

	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("firehydrant: %s: status %d: %s", op, resp.StatusCode, snippet)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
