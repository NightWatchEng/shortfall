// Package incidentio writes a shortfall impact summary into an incident.io
// incident's custom field. incident.io is a consumer of the number, not a
// producer: this is a thin writer over the V2 Edit action — a plain
// *http.Client, no SDK, base URL overridable so tests run against httptest.
package incidentio

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

const defaultBaseURL = "https://api.incident.io"

// Client writes impact summaries to incident.io.
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

// New builds a Client for an API key and the text custom field the impact
// summary is written into (create one in incident.io settings; its id is on
// the field's page).
func New(token, customFieldID string, opts ...Option) *Client {
	c := &Client{
		http:          &http.Client{Timeout: 10 * time.Second},
		token:         token,
		baseURL:       defaultBaseURL,
		customFieldID: customFieldID,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WriteImpact sets the configured custom field to the report's impact
// summary (report.Summary) via POST /v2/incidents/{id}/actions/edit. The
// edit never notifies the incident channel — the number updates quietly.
func (c *Client) WriteImpact(ctx context.Context, incidentID string, r engine.Report) error {
	type value struct {
		ValueText string `json:"value_text"`
	}
	type entry struct {
		CustomFieldID string  `json:"custom_field_id"`
		Values        []value `json:"values"`
	}
	body := map[string]any{
		"incident": map[string]any{
			"custom_field_entries": []entry{{
				CustomFieldID: c.customFieldID,
				Values:        []value{{ValueText: report.Summary(r)}},
			}},
		},
		"notify_incident_channel": false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v2/incidents/%s/actions/edit", c.baseURL, incidentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("incidentio: edit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("incidentio: edit: status %d", resp.StatusCode)
	}
	return nil
}
