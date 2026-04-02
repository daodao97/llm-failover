package failover

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeHTTPPostRetriesWhenRetryConfigured(t *testing.T) {
	attempts := 0
	ch := Channel{
		Id:      201,
		Name:    "retry-post",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"429"}}`)),
			}, nil
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry: RetryConfig{
			MaxAttempts: 3,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusTooManyRequests
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if attempts != 3 {
		t.Fatalf("attempts=%d, want=3", attempts)
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusTooManyRequests)
	}
}

func TestServeHTTPPostNoRetryWhenMaxAttemptsOne(t *testing.T) {
	attempts := 0
	ch := Channel{
		Id:      202,
		Name:    "no-retry-post",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"429"}}`)),
			}, nil
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry: RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusTooManyRequests
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if attempts != 1 {
		t.Fatalf("attempts=%d, want=1", attempts)
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusTooManyRequests)
	}
}

func TestServeHTTPPostChannelFailoverWhenRetryConfigured(t *testing.T) {
	firstAttempts := 0
	secondAttempts := 0
	channels := []Channel{
		{
			Id:      204,
			Name:    "first",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k1", Value: "v1"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				firstAttempts++
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"429"}}`)),
				}, nil
			},
		},
		{
			Id:      205,
			Name:    "second",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k2", Value: "v2"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				secondAttempts++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				}, nil
			},
		},
	}

	p := New(Config{
		Channels: channels,
		Retry: RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusTooManyRequests
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if firstAttempts != 1 {
		t.Fatalf("firstAttempts=%d, want=1", firstAttempts)
	}
	if secondAttempts != 1 {
		t.Fatalf("secondAttempts=%d, want=1", secondAttempts)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusOK)
	}
}
