package httpbatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scriptDoer returns a scripted sequence of (status, err) per call.
type scriptDoer struct {
	steps []step
	calls int
	gotCT []string
}

type step struct {
	status int
	err    error
}

func (d *scriptDoer) Do(req *http.Request) (*http.Response, error) {
	d.gotCT = append(d.gotCT, req.Header.Get("Content-Type"))
	i := d.calls
	d.calls++
	if i >= len(d.steps) {
		i = len(d.steps) - 1
	}
	s := d.steps[i]
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func TestPostRetryBehavior(t *testing.T) {
	netErr := errors.New("connection refused")
	cases := []struct {
		name      string
		steps     []step
		maxRetry  int
		wantCalls int
		wantErr   bool
	}{
		{"success first try", []step{{status: 200}}, 3, 1, false},
		{"retry on 503 then succeed", []step{{status: 503}, {status: 200}}, 3, 2, false},
		{"retry on 429 then succeed", []step{{status: 429}, {status: 200}}, 3, 2, false},
		{"retry on network error then succeed", []step{{err: netErr}, {status: 200}}, 3, 2, false},
		{"permanent 400 not retried", []step{{status: 400}}, 3, 1, true},
		{"permanent 404 not retried", []step{{status: 404}}, 3, 1, true},
		{"exhaust retries on persistent 500", []step{{status: 500}}, 2, 3, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &scriptDoer{steps: c.steps}
			var slept []time.Duration
			cl := New("http://example.invalid/x",
				WithHTTPClient(d),
				WithRetry(c.maxRetry, 100*time.Millisecond),
				withSleep(func(_ context.Context, dur time.Duration) error {
					slept = append(slept, dur)
					return nil
				}),
			)
			err := cl.Post(context.Background(), "application/json", []byte(`{}`))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if d.calls != c.wantCalls {
				t.Fatalf("calls = %d, want %d", d.calls, c.wantCalls)
			}
			// A sleep precedes every retry (calls-1 backoffs).
			if len(slept) != c.wantCalls-1 {
				t.Fatalf("backoffs = %d, want %d", len(slept), c.wantCalls-1)
			}
		})
	}
}

func TestPostExponentialBackoff(t *testing.T) {
	d := &scriptDoer{steps: []step{{status: 503}, {status: 503}, {status: 503}, {status: 200}}}
	var slept []time.Duration
	cl := New("http://example.invalid/x",
		WithHTTPClient(d),
		WithRetry(3, 100*time.Millisecond),
		withSleep(func(_ context.Context, dur time.Duration) error { slept = append(slept, dur); return nil }),
	)
	if err := cl.Post(context.Background(), "application/json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(slept) != len(want) {
		t.Fatalf("backoffs = %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("backoff[%d] = %v, want %v (doubling)", i, slept[i], want[i])
		}
	}
}

func TestPostHonorsContextCancellation(t *testing.T) {
	d := &scriptDoer{steps: []step{{status: 503}}}
	cl := New("http://example.invalid/x",
		WithHTTPClient(d),
		WithRetry(5, time.Second),
		withSleep(func(ctx context.Context, _ time.Duration) error { return context.Canceled }),
	)
	err := cl.Post(context.Background(), "application/json", []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("should stop after the first failure when the backoff is cancelled, calls = %d", d.calls)
	}
}

func TestPostSetsHeadersAndContentType(t *testing.T) {
	d := &scriptDoer{steps: []step{{status: 200}}}
	cl := New("http://example.invalid/x", WithHTTPClient(d), WithHeader("Authorization", "Splunk tok"))
	if err := cl.Post(context.Background(), "application/json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if d.gotCT[0] != "application/json" {
		t.Fatalf("content-type = %q", d.gotCT[0])
	}
}
