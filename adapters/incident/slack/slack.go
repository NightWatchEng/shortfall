// Copyright 2026 Yauvan Suba
// SPDX-License-Identifier: Apache-2.0

// Package slack posts a shortfall impact report to a Slack channel and keeps it
// fresh while an incident is open.
//
// It posts the text ledger block (engine/report.RenderText) inside a code fence
// so the columns line up, remembers the message timestamp, and edits that same
// message on each refresh rather than spamming the channel. It speaks the Slack
// Web API over a plain *http.Client (no SDK dependency); the base URL is
// overridable so tests run against httptest with no network.
package slack

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

// defaultBaseURL is Slack's Web API root.
const defaultBaseURL = "https://slack.com/api"

// Client posts and refreshes impact reports in Slack.
type Client struct {
	http    *http.Client
	token   string
	baseURL string
}

// Option configures New.
type Option func(*Client)

// WithHTTPClient sets the HTTP client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithBaseURL overrides the Slack API base URL (test seam).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// New builds a Client for a bot token (xoxb-...).
func New(token string, opts ...Option) *Client {
	c := &Client{http: &http.Client{Timeout: 10 * time.Second}, token: token, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Post posts the report's text ledger block to channel and returns the message
// timestamp (ts) — pass it to Update to refresh the same message.
func (c *Client) Post(ctx context.Context, channel string, r engine.Report) (string, error) {
	var out struct {
		OK    bool   `json:"ok"`
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if err := c.call(ctx, "chat.postMessage", map[string]string{
		"channel": channel, "text": fence(r),
	}, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("slack: chat.postMessage failed: %s", out.Error)
	}
	return out.TS, nil
}

// Update edits the message at ts with a fresh render of r.
func (c *Client) Update(ctx context.Context, channel, ts string, r engine.Report) error {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.call(ctx, "chat.update", map[string]string{
		"channel": channel, "ts": ts, "text": fence(r),
	}, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack: chat.update failed: %s", out.Error)
	}
	return nil
}

// Refresh posts r once, then re-renders and edits that message every interval
// until ctx is done or fetch reports the incident closed (open=false). fetch
// returns the latest report; a fetch error stops the loop and is returned.
// A non-positive interval is rejected — no silent no-op ticker.
func (c *Client) Refresh(ctx context.Context, channel string, r engine.Report, interval time.Duration, fetch func(context.Context) (engine.Report, bool, error)) error {
	if interval <= 0 {
		return fmt.Errorf("slack: refresh interval %v must be positive", interval)
	}
	ts, err := c.Post(ctx, channel, r)
	if err != nil {
		return err
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			next, open, err := fetch(ctx)
			if err != nil {
				return err
			}
			if !open {
				return nil // incident closed; leave the last render in place
			}
			if err := c.Update(ctx, channel, ts, next); err != nil {
				return err
			}
		}
	}
}

// fence wraps the text ledger block in a Slack code fence so its columns align.
func fence(r engine.Report) string {
	return "```\n" + report.RenderText(r) + "\n```"
}

// call POSTs a JSON body to a Slack Web API method and decodes the response.
func (c *Client) call(ctx context.Context, method string, body map[string]string, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack: %s: HTTP %d", method, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("slack: %s: decode: %w", method, err)
	}
	return nil
}
