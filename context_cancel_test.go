package failover

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func TestServeHTTPAttemptTimeoutFailsOverToNextChannel(t *testing.T) {
	firstAttempts := 0
	secondAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "slow",
			BaseURL: "https://slow.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "slow-key", Value: "slow-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				firstAttempts++
				<-ctx.Request.Context().Done()
				return nil, ctx.Request.Context().Err()
			},
		},
		{
			Id:      2,
			Name:    "backup",
			BaseURL: "https://backup.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "backup-key", Value: "backup-value"}}
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

	p := New(Config{
		Channels:        channels,
		Retry:           NoRetry(),
		FailoverTimeout: 200 * time.Millisecond,
		AttemptTimeout:  20 * time.Millisecond,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	p.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if firstAttempts != 1 {
		t.Fatalf("firstAttempts=%d, want=1（attempt 超时不应在同 key 上重试）", firstAttempts)
	}
	if secondAttempts != 1 {
		t.Fatalf("secondAttempts=%d, want=1（attempt 超时应切到下一个渠道）", secondAttempts)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("ServeHTTP should fail over near attempt timeout, elapsed=%s", elapsed)
	}
}

func TestServeHTTPAttemptTimeoutSkipsSameKeyRetryAndRotatesKeys(t *testing.T) {
	attemptsByKey := map[string]int{}
	channels := []Channel{
		{
			Id:      1,
			Name:    "slow",
			BaseURL: "https://slow.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{
					{ID: "key-a", Value: "value-a"},
					{ID: "key-b", Value: "value-b"},
				}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				attemptsByKey[ctx.CurrentKey.ID]++
				<-ctx.Request.Context().Done()
				return nil, ctx.Request.Context().Err()
			},
		},
	}

	retry := DefaultRetry()
	retry.MaxAttempts = 3
	p := New(Config{
		Channels:        channels,
		Retry:           retry,
		FailoverTimeout: 300 * time.Millisecond,
		AttemptTimeout:  20 * time.Millisecond,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want=%d body=%s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	// attempt 超时跳过同 key 重试（MaxAttempts=3 不生效），但仍轮换池内每个 key 一次
	if attemptsByKey["key-a"] != 1 || attemptsByKey["key-b"] != 1 {
		t.Fatalf("attemptsByKey=%v, want one attempt per key", attemptsByKey)
	}
}

func TestTryChannelsAttemptTimeoutRecordsCircuitFailure(t *testing.T) {
	slowAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "slow",
			BaseURL: "https://slow.example.com",
			Enabled: true,
			CType:   CTypeThird,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "slow-key", Value: "slow-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				slowAttempts++
				<-ctx.Request.Context().Done()
				return nil, ctx.Request.Context().Err()
			},
		},
		{
			Id:      2,
			Name:    "good",
			BaseURL: "https://good.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "good-key", Value: "good-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				goodAttempts++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
			},
		},
	}

	p := New(Config{
		Retry:           NoRetry(),
		FailoverTimeout: 500 * time.Millisecond,
		AttemptTimeout:  20 * time.Millisecond,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel, lastErr=%v", i+1, result.lastErr)
		}
		result.successResp.Body.Close()
	}

	// attempt 超时计入熔断：两次失败后 slow 渠道应被熔断跳过
	if slowAttempts != 2 {
		t.Fatalf("slowAttempts=%d, want=2 after circuit opens on attempt timeouts", slowAttempts)
	}
	if goodAttempts != 3 {
		t.Fatalf("goodAttempts=%d, want=3", goodAttempts)
	}
}

func TestTryChannelsFailoverTimeoutDoesNotRecordCircuitFailure(t *testing.T) {
	cases := []struct {
		name           string
		attemptTimeout time.Duration
	}{
		{name: "attempt timeout unset"},
		{name: "attempt timeout larger than failover budget", attemptTimeout: 100 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slowAttempts := 0
			channels := []Channel{
				{
					Id:      1,
					Name:    "slow",
					BaseURL: "https://slow.example.com",
					Enabled: true,
					CType:   CTypeThird,
					GetKeys: func(ctx *Context) []Key {
						return []Key{{ID: "slow-key", Value: "slow-value"}}
					},
					Handler: func(ctx *Context) (*http.Response, error) {
						slowAttempts++
						<-ctx.Request.Context().Done()
						return nil, ctx.Request.Context().Err()
					},
				},
			}

			p := New(Config{
				Retry:           NoRetry(),
				FailoverTimeout: 20 * time.Millisecond,
				AttemptTimeout:  tc.attemptTimeout,
				CircuitBreaker: CircuitBreakerConfig{
					Enabled:            true,
					MinSamples:         1,
					ErrorRateThreshold: 1,
					FailureWindow:      time.Second,
					Cooldown:           time.Minute,
				},
			})

			for i := 0; i < 2; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
				ctx := &Context{Request: req}
				result := p.tryChannels(req, ctx, channels, NoRetry())
				if result.successResp != nil {
					result.successResp.Body.Close()
					t.Fatalf("request %d should not succeed", i+1)
				}
				if result.lastErr == nil {
					t.Fatalf("request %d expected failover timeout", i+1)
				}
				if IsAttemptTimeoutError(result.lastErr) {
					t.Fatalf("request %d lastErr=%v, want failover timeout not attempt timeout", i+1, result.lastErr)
				}
				if !IsContextDeadlineExceededError(result.lastErr) {
					t.Fatalf("request %d lastErr=%v, want context deadline exceeded", i+1, result.lastErr)
				}
			}

			if slowAttempts != 2 {
				t.Fatalf("slowAttempts=%d, want=2 because failover timeouts must not open circuit", slowAttempts)
			}
			if allowed, _, _ := p.breaker.Allow(&channels[0]); !allowed {
				t.Fatalf("channel should remain allowed after failover timeout")
			}
		})
	}
}

func TestTryChannelsParentDeadlineDoesNotBecomeAttemptTimeout(t *testing.T) {
	slowAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "slow",
			BaseURL: "https://slow.example.com",
			Enabled: true,
			CType:   CTypeThird,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "slow-key", Value: "slow-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				slowAttempts++
				<-ctx.Request.Context().Done()
				return nil, ctx.Request.Context().Err()
			},
		},
	}

	p := New(Config{
		Retry:          NoRetry(),
		AttemptTimeout: 100 * time.Millisecond,
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
	})

	for i := 0; i < 2; i++ {
		parentCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`)).WithContext(parentCtx)
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, NoRetry())
		cancel()

		if result.successResp != nil {
			result.successResp.Body.Close()
			t.Fatalf("request %d should not succeed", i+1)
		}
		if result.lastErr == nil {
			t.Fatalf("request %d expected parent deadline timeout", i+1)
		}
		if IsAttemptTimeoutError(result.lastErr) {
			t.Fatalf("request %d lastErr=%v, want parent deadline not attempt timeout", i+1, result.lastErr)
		}
		if !IsContextDeadlineExceededError(result.lastErr) {
			t.Fatalf("request %d lastErr=%v, want context deadline exceeded", i+1, result.lastErr)
		}
	}

	if slowAttempts != 2 {
		t.Fatalf("slowAttempts=%d, want=2 because parent deadlines must not open circuit", slowAttempts)
	}
	if allowed, _, _ := p.breaker.Allow(&channels[0]); !allowed {
		t.Fatalf("channel should remain allowed after parent deadline")
	}
}

func TestTryChannelsStopsWhenRemainingBudgetBelowMinimum(t *testing.T) {
	firstAttempts := 0
	secondAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "first",
			BaseURL: "https://first.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "first-key", Value: "first-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				firstAttempts++
				time.Sleep(40 * time.Millisecond)
				return nil, errors.New("temporary upstream failure")
			},
		},
		{
			Id:      2,
			Name:    "second",
			BaseURL: "https://second.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "second-key", Value: "second-value"}}
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

	p := New(Config{
		Retry:             DefaultRetry(),
		FailoverTimeout:   60 * time.Millisecond,
		MinAttemptTimeout: 50 * time.Millisecond,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}

	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp != nil {
		result.successResp.Body.Close()
		t.Fatalf("response should be nil when remaining budget is below minimum")
	}
	if result.lastErr == nil || !IsContextDeadlineExceededError(result.lastErr) {
		t.Fatalf("lastErr=%v, want context deadline exceeded", result.lastErr)
	}
	if firstAttempts != 1 {
		t.Fatalf("firstAttempts=%d, want=1", firstAttempts)
	}
	if secondAttempts != 0 {
		t.Fatalf("secondAttempts=%d, want=0", secondAttempts)
	}
}

func TestAttemptTimeoutDoesNotCancelSuccessfulResponseBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	ch := &Channel{
		Id:      1,
		Name:    "stream-like",
		BaseURL: upstream.URL,
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "key", Value: "value"}}
		},
	}
	p := New(Config{
		Retry:          NoRetry(),
		AttemptTimeout: 10 * time.Millisecond,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}

	resp, err := p.tryChannel(req, ctx, ch, p.cfg.Retry)
	if err != nil {
		t.Fatalf("tryChannel error: %v", err)
	}
	if resp == nil {
		t.Fatalf("response should not be nil")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body should remain readable after attempt timeout window: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body=%q, want ok", string(body))
	}
}
