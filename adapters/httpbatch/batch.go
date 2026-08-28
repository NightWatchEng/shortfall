// Package httpbatch is the shared HTTP plumbing for shortfall's HTTP-push
// exporters (Splunk HEC, Loki, Datadog): a small POST client with bounded
// exponential-backoff retry that distinguishes retryable failures (network
// errors, 429, 5xx) from permanent ones (other 4xx), and honors context
// cancellation between attempts. It holds no shortfall types and no
// third-party dependencies — each exporter maps its own payload and calls
// Post.
package httpbatch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Doer is the slice of *http.Client this package needs — an interface so
// tests substitute a transport with no network.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client POSTs bodies to a fixed endpoint with retry/backoff.
type Client struct {
	endpoint   string
	header     http.Header
	doer       Doer
	maxRetries int
	baseDelay  time.Duration
	// sleep waits d or returns early if ctx is cancelled; a seam so tests
	// neither sleep nor lose the cancellation semantics.
	sleep func(ctx context.Context, d time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP doer (default: a *http.Client with a 30s
// timeout).
func WithHTTPClient(d Doer) Option { return func(c *Client) { c.doer = d } }

// WithHeader adds a header sent on every request (e.g. Authorization).
func WithHeader(key, value string) Option {
	return func(c *Client) { c.header.Add(key, value) }
}

// WithRetry sets the max retry count (attempts beyond the first) and the
// base backoff delay (doubled each retry). max<0 is treated as 0.
func WithRetry(max int, base time.Duration) Option {
	return func(c *Client) {
		if max < 0 {
			max = 0
		}
		c.maxRetries, c.baseDelay = max, base
	}
}

// withSleep overrides the backoff sleeper (test seam).
func withSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// New builds a Client for endpoint.
func New(endpoint string, opts ...Option) *Client {
	c := &Client{
		endpoint:   endpoint,
		header:     http.Header{},
		doer:       &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
		baseDelay:  200 * time.Millisecond,
		sleep:      sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// sleepCtx waits d, returning ctx.Err() if the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Post sends body with the given content type, retrying retryable failures
// (network errors, HTTP 429, HTTP 5xx) with exponential backoff. A non-429
// 4xx is permanent and returned immediately. Returns nil on the first 2xx.
func (c *Client) Post(ctx context.Context, contentType string, body []byte) error {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.baseDelay << (attempt - 1)
			if err := c.sleep(ctx, delay); err != nil {
				return fmt.Errorf("httpbatch: %w", err)
			}
		}
		status, err := c.attempt(ctx, contentType, body)
		switch {
		case err != nil:
			lastErr = err // network/transport error — retryable
		case status < 300:
			return nil // success
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = fmt.Errorf("httpbatch: retryable status %d", status)
		default:
			return fmt.Errorf("httpbatch: permanent status %d", status)
		}
	}
	return fmt.Errorf("httpbatch: retries exhausted (%d): %w", c.maxRetries, lastErr)
}

// attempt does one request and returns the status code, draining and closing
// the body so the connection can be reused.
func (c *Client) attempt(ctx context.Context, contentType string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("httpbatch: new request: %w", err)
	}
	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.doer.Do(req)
	if err != nil {
		return 0, fmt.Errorf("httpbatch: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
