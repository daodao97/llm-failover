package failover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsRetryableErrorWrappedContextCanceled(t *testing.T) {
	err := fmt.Errorf("wrapped transport error: %w", context.Canceled)
	if IsRetryableError(err) {
		t.Fatalf("wrapped context canceled should not be retryable")
	}
}

func TestIsRetryableErrorPoolExhausted(t *testing.T) {
	cases := []error{
		errors.New("no keys available"),
		errors.New("no keys available: all keys concurrency limited"),
		errors.New("key concurrency limit reached"),
		fmt.Errorf("[ABC=] %w", errors.New("no keys available")),
	}

	for _, err := range cases {
		if IsRetryableError(err) {
			t.Fatalf("error %q should not be retryable", err)
		}
	}
}

func TestTryChannelStopsAfterContextCanceled(t *testing.T) {
	attempts := 0
	ch := &Channel{
		Id:      1,
		Name:    "first",
		BaseURL: "https://example.com",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}, {ID: "k2", Value: "v2"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			attempts++
			return nil, fmt.Errorf("wrapped context canceled: %w", context.Canceled)
		},
	}

	p := New(Config{Retry: DefaultRetry()})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}

	resp, err := p.tryChannel(req, ctx, ch, DefaultRetry())
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("response should be nil when request is canceled")
	}
	if err == nil {
		t.Fatalf("expected context canceled error")
	}
	if !IsContextCanceledError(err) {
		t.Fatalf("error=%v, want context canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want=1", attempts)
	}
}

func TestTryChannelsStopsAfterContextCanceled(t *testing.T) {
	firstAttempts := 0
	secondAttempts := 0

	channels := []Channel{
		{
			Id:      1,
			Name:    "first",
			BaseURL: "https://example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k1", Value: "v1"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				firstAttempts++
				return nil, fmt.Errorf("wrapped context canceled: %w", context.Canceled)
			},
		},
		{
			Id:      2,
			Name:    "second",
			BaseURL: "https://example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k2", Value: "v2"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				secondAttempts++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
			},
		},
	}

	p := New(Config{Retry: DefaultRetry()})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}

	result := p.tryChannels(req, ctx, channels, DefaultRetry())
	if result.successResp != nil {
		result.successResp.Body.Close()
		t.Fatalf("response should be nil when request is canceled")
	}
	if result.lastErr == nil {
		t.Fatalf("expected context canceled error")
	}
	if !IsContextCanceledError(result.lastErr) {
		t.Fatalf("error=%v, want context canceled", result.lastErr)
	}
	if firstAttempts != 1 {
		t.Fatalf("firstAttempts=%d, want=1", firstAttempts)
	}
	if secondAttempts != 0 {
		t.Fatalf("secondAttempts=%d, want=0", secondAttempts)
	}
}

func TestTryChannelNoKeysAvailableFastFail(t *testing.T) {
	ch := &Channel{
		Id:      1,
		Name:    "kiro-pool",
		BaseURL: "https://example.com",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			ctx.PoolStats = PoolStats{
				TotalCandidates: 1,
				Selected:        1,
				Returned:        0,
			}
			return nil
		},
	}

	p := New(Config{Retry: DefaultRetry()})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}

	start := time.Now()
	resp, err := p.tryChannel(req, ctx, ch, RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   200 * time.Millisecond,
		MaxDelay:    time.Second,
		RetryOnError: func(ctx *Context, ch *Channel, err error) bool {
			return IsRetryableError(err)
		},
	})
	elapsed := time.Since(start)

	if resp != nil {
		resp.Body.Close()
		t.Fatal("response should be nil")
	}
	if err == nil || !strings.Contains(err.Error(), "no keys available") {
		t.Fatalf("unexpected err=%v", err)
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("tryChannel should fast fail on no keys, elapsed=%s", elapsed)
	}
}
