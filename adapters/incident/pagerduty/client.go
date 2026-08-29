// Package pagerduty writes a shortfall impact summary into a PagerDuty
// incident's custom field. PagerDuty is a consumer of the number, not a
// producer: a thin writer over the REST custom-fields endpoint — plain
// *http.Client, no SDK, base URL overridable so tests run against httptest.
//
// AttachCustomersCSV posts the top-accounts CSV as an incident note; note
// writes need the requester's email (PagerDuty's required From header).
package pagerduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/NightWatchEng/shortfall/engine"
	"github.com/NightWatchEng/shortfall/engine/report"
)

const defaultBaseURL = "https://api.pagerduty.com"

// Client writes impact summaries to PagerDuty.
type Client struct {
	http      *http.Client
	token     string
	baseURL   string
	fieldName string
	fromEmail string
}

// Option configures New.
type Option func(*Client)

// WithHTTPClient sets the HTTP client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithBaseURL overrides the API base URL (test seam).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// New builds a Client for an API token, the string custom field the impact
// summary is written into (by field name), and the requester email note
// writes are attributed to.
func New(token, fieldName, fromEmail string, opts ...Option) *Client {
	c := &Client{
		http:      &http.Client{Timeout: 10 * time.Second},
		token:     token,
		baseURL:   defaultBaseURL,
		fieldName: fieldName,
		fromEmail: fromEmail,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WriteImpact sets the configured custom field to the report's impact
// summary via PUT /incidents/{id}/custom_fields/values.
func (c *Client) WriteImpact(ctx context.Context, incidentID string, r engine.Report) error {
	body := map[string]any{
		"custom_fields": []map[string]any{{
			"name": c.fieldName, "value": report.Summary(r),
		}},
	}
	path := fmt.Sprintf("/incidents/%s/custom_fields/values", incidentID)
	return c.call(ctx, http.MethodPut, path, body, false, "custom fields")
}

// AttachCustomersCSV posts the customers leg's top accounts as an incident
// note (with the required From header). A leg that says why it is
// unavailable propagates that reason instead of posting an empty file.
func (c *Client) AttachCustomersCSV(ctx context.Context, incidentID string, r engine.Report) error {
	csv, err := report.CustomersCSV(r)
	if err != nil {
		return fmt.Errorf("pagerduty: %w", err)
	}
	body := map[string]any{
		"note": map[string]any{
			"content": "shortfall customers (top accounts, minor units):\n" + string(csv),
		},
	}
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/incidents/%s/notes", incidentID), body, true, "note")
}

func (c *Client) call(ctx context.Context, method, path string, body map[string]any, withFrom bool, op string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token token="+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if withFrom {
		req.Header.Set("From", c.fromEmail)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("pagerduty: %s: status %d", op, resp.StatusCode)
	}
	return nil
}
