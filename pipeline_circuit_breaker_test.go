package failover

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const retryableBusinessMarker = "retryable business failure"

func writeSuccessfulPipelineResult(t *testing.T, p *Proxy, req *http.Request, ctx *Context, result pipelineResult) {
	t.Helper()
	p.writePipelineResponse(httptest.NewRecorder(), req, ctx, result)
}

func TestTryChannelsOpensCircuitAfterBurstFailures(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ch != nil && ch.Name == "bad"
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 after circuit opens", badAttempts)
	}
	if goodAttempts != 3 {
		t.Fatalf("goodAttempts=%d, want=3", goodAttempts)
	}
}

func TestTryChannelsCircuitBreakerWhitelistByName(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
		CircuitBreakerWhitelist: []string{"bad", "good"},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ch != nil && ch.Name == "bad"
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if badAttempts != 3 {
		t.Fatalf("badAttempts=%d, want=3 because whitelisted channel should not open circuit", badAttempts)
	}
	if goodAttempts != 3 {
		t.Fatalf("goodAttempts=%d, want=3", goodAttempts)
	}
	var badSnapshot *ChannelHealthSnapshot
	var goodSnapshot *ChannelHealthSnapshot
	for _, snapshot := range p.breaker.Snapshot() {
		if snapshot.ChannelName == "bad" {
			snapshot := snapshot
			badSnapshot = &snapshot
		}
		if snapshot.ChannelName == "good" {
			snapshot := snapshot
			goodSnapshot = &snapshot
		}
	}
	if badSnapshot == nil {
		t.Fatal("whitelisted channel should be recorded in breaker snapshot")
	}
	if !badSnapshot.CircuitBreakerWhitelisted {
		t.Fatalf("circuitBreakerWhitelisted=%v, want=true", badSnapshot.CircuitBreakerWhitelisted)
	}
	if badSnapshot.Status != "closed" {
		t.Fatalf("whitelisted status=%s, want=closed", badSnapshot.Status)
	}
	if badSnapshot.RequestCount != 3 {
		t.Fatalf("whitelisted requestCount=%d, want=3", badSnapshot.RequestCount)
	}
	if badSnapshot.SuccessCount != 0 {
		t.Fatalf("whitelisted successCount=%d, want=0", badSnapshot.SuccessCount)
	}
	if badSnapshot.FailureCount != 3 {
		t.Fatalf("whitelisted failureCount=%d, want=3", badSnapshot.FailureCount)
	}
	if goodSnapshot == nil {
		t.Fatal("successful whitelisted channel should be recorded in breaker snapshot")
	}
	if !goodSnapshot.CircuitBreakerWhitelisted {
		t.Fatalf("good circuitBreakerWhitelisted=%v, want=true", goodSnapshot.CircuitBreakerWhitelisted)
	}
	if goodSnapshot.Status != "closed" {
		t.Fatalf("good status=%s, want=closed", goodSnapshot.Status)
	}
	if goodSnapshot.RequestCount != 3 {
		t.Fatalf("good requestCount=%d, want=3", goodSnapshot.RequestCount)
	}
	if goodSnapshot.SuccessCount != 3 {
		t.Fatalf("good successCount=%d, want=3", goodSnapshot.SuccessCount)
	}
	if goodSnapshot.FailureCount != 0 {
		t.Fatalf("good failureCount=%d, want=0", goodSnapshot.FailureCount)
	}
}

func TestTryChannelsOpensCircuitWithinSingleRequestAfterRetryBurst(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"429"}}`)),
				}, nil
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
		Retry: RetryConfig{
			MaxAttempts: 5,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusTooManyRequests
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         3,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp == nil {
		t.Fatalf("first request should succeed on fallback channel")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx = &Context{Request: req}
	result = p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp == nil {
		t.Fatalf("second request should skip open circuit and succeed on fallback channel")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)

	if badAttempts != 3 {
		t.Fatalf("badAttempts=%d, want=3 because breaker should open during first retry burst", badAttempts)
	}
	if goodAttempts != 2 {
		t.Fatalf("goodAttempts=%d, want=2", goodAttempts)
	}
}

func TestTryChannelsRecoversAfterCooldownProbeSuccess(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	recoverBad := false
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				if recoverBad {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           20 * time.Millisecond,
		},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ch != nil && ch.Name == "bad"
		},
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, NoRetry())
	if result.successResp == nil {
		t.Fatalf("request during open circuit should use fallback channel")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)

	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 before cooldown expires", badAttempts)
	}

	time.Sleep(40 * time.Millisecond)
	recoverBad = true

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx = &Context{Request: req}
	result = p.tryChannels(req, ctx, channels, NoRetry())
	if result.successResp == nil {
		t.Fatalf("probe request should recover bad channel")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)

	if badAttempts != 3 {
		t.Fatalf("badAttempts=%d, want=3 after recovery probe", badAttempts)
	}
	if goodAttempts != 3 {
		t.Fatalf("goodAttempts=%d, want=3 because recovered channel should stop fallback", goodAttempts)
	}
}

func TestTryChannelsHalfOpenProbeDoesNotHideFailureWithRetries(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				status := http.StatusTooManyRequests
				if badAttempts >= 3 {
					status = http.StatusOK
				}
				return &http.Response{
					StatusCode: status,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"429"}}`)),
				}, nil
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
		Retry: RetryConfig{
			MaxAttempts: 3,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusTooManyRequests
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           20 * time.Millisecond,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp == nil {
		t.Fatalf("first request should succeed on fallback channel")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)

	time.Sleep(40 * time.Millisecond)

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx = &Context{Request: req}
	result = p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp == nil {
		t.Fatalf("probe request should still fall back after first probe failure")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)

	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 because half-open probe should stop after first failed attempt", badAttempts)
	}
	if goodAttempts != 2 {
		t.Fatalf("goodAttempts=%d, want=2 because fallback should be used for both requests", goodAttempts)
	}

	snapshots := p.breaker.Snapshot()
	if len(snapshots) != 2 {
		t.Fatalf("snapshot len=%d, want=2", len(snapshots))
	}
	for _, snapshot := range snapshots {
		if snapshot.ChannelName == "bad" && snapshot.Status != "open" {
			t.Fatalf("bad channel status=%s, want=open", snapshot.Status)
		}
	}
}

func TestTryChannelsProbeUncountedFailureReleasesHalfOpen(t *testing.T) {
	// 探测拿到 400（默认不计入熔断的失败）不应让渠道永久卡在半开状态
	testProbeInconclusiveReleasesHalfOpen(t, func() (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       http.NoBody,
		}, nil
	})
}

func TestTryChannelsProbeClientCancelReleasesHalfOpen(t *testing.T) {
	// 探测期间客户端取消（不计入熔断）不应让渠道永久卡在半开状态
	testProbeInconclusiveReleasesHalfOpen(t, func() (*http.Response, error) {
		return nil, context.Canceled
	})
}

// testProbeInconclusiveReleasesHalfOpen 验证半开探测结果"无结论"（失败但未计入熔断）时，
// halfOpen 会被复位，后续请求可以重新发起探测并最终恢复渠道。
func testProbeInconclusiveReleasesHalfOpen(t *testing.T, probeResult func() (*http.Response, error)) {
	t.Helper()
	badAttempts := 0
	goodAttempts := 0
	badMode := "fail" // fail -> probe -> ok
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				switch badMode {
				case "probe":
					return probeResult()
				case "ok":
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusBadGateway,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				}
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           20 * time.Millisecond,
		},
	})

	// 两次 502 失败打开熔断
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}
	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 before circuit opens", badAttempts)
	}

	// 冷却结束后第一次探测：失败但不计入熔断（无结论）
	time.Sleep(40 * time.Millisecond)
	badMode = "probe"
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, NoRetry())
	if result.successResp != nil {
		t.Fatalf("inconclusive probe request should not succeed")
	}
	p.writePipelineResponse(httptest.NewRecorder(), req, ctx, result)
	if badAttempts != 3 {
		t.Fatalf("badAttempts=%d, want=3 because probe should reach bad channel", badAttempts)
	}

	// 无结论的探测不应让渠道卡死在半开状态：下一个请求应能再次探测并恢复渠道
	badMode = "ok"
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx = &Context{Request: req}
	result = p.tryChannels(req, ctx, channels, NoRetry())
	if result.successResp == nil {
		t.Fatalf("recovery probe should succeed")
	}
	writeSuccessfulPipelineResult(t, p, req, ctx, result)
	if badAttempts != 4 {
		t.Fatalf("badAttempts=%d, want=4: channel stuck in half-open after inconclusive probe", badAttempts)
	}
	if goodAttempts != 2 {
		t.Fatalf("goodAttempts=%d, want=2", goodAttempts)
	}
}

func TestTryChannelsPoolInternalKeyFailureDoesNotOpenChannelCircuit(t *testing.T) {
	poolBadKeyAttempts := 0
	poolGoodKeyAttempts := 0
	fallbackAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "pool",
			BaseURL: "https://pool.example.com",
			Enabled: true,
			CType:   CTypePool,
			GetKeys: func(ctx *Context) []Key {
				return []Key{
					{ID: "bad-key", Value: "bad-value"},
					{ID: "good-key", Value: "good-value"},
				}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				switch ctx.CurrentKey.ID {
				case "bad-key":
					poolBadKeyAttempts++
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"429"}}`)),
					}, nil
				case "good-key":
					poolGoodKeyAttempts++
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				default:
					t.Fatalf("unexpected key id: %s", ctx.CurrentKey.ID)
					return nil, nil
				}
			},
		},
		{
			Id:      2,
			Name:    "fallback",
			BaseURL: "https://fallback.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "fallback-key", Value: "fallback-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				fallbackAttempts++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
			},
		},
	}

	p := New(Config{
		Retry: RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusTooManyRequests
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 0.5,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
		if result.successResp == nil {
			t.Fatalf("request %d should succeed within pool channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if poolBadKeyAttempts != 3 {
		t.Fatalf("poolBadKeyAttempts=%d, want=3", poolBadKeyAttempts)
	}
	if poolGoodKeyAttempts != 3 {
		t.Fatalf("poolGoodKeyAttempts=%d, want=3", poolGoodKeyAttempts)
	}
	if fallbackAttempts != 0 {
		t.Fatalf("fallbackAttempts=%d, want=0 because pool channel should stay healthy", fallbackAttempts)
	}
}

func TestTryChannelsPoolNoKeysConcurrencyLimitedDoesNotOpenCircuit(t *testing.T) {
	poolAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "kiro-pool",
			BaseURL: "https://pool.example.com",
			Enabled: true,
			CType:   CTypePool,
			GetKeys: func(ctx *Context) []Key {
				poolAttempts++
				ctx.PoolStats = PoolStats{
					TotalCandidates: 2,
					Busy:            2,
				}
				return nil
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ctx != nil &&
				ctx.LastStatusCode == http.StatusBadRequest &&
				strings.Contains(strings.ToLower(string(ctx.LastResponseBody)), retryableBusinessMarker)
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if poolAttempts != 3 {
		t.Fatalf("poolAttempts=%d, want=3 because pool no-keys errors should not open circuit", poolAttempts)
	}
	if goodAttempts != 3 {
		t.Fatalf("goodAttempts=%d, want=3", goodAttempts)
	}
}

func TestTryChannelsStatus400DoesNotOpenCircuit(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad-request-channel",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid request"}}`)),
				}, nil
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
		Retry: RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusBadRequest &&
					strings.Contains(strings.ToLower(string(body)), retryableBusinessMarker)
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ctx != nil &&
				ctx.LastStatusCode == http.StatusBadRequest &&
				strings.Contains(strings.ToLower(string(ctx.LastResponseBody)), retryableBusinessMarker)
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
		if result.successResp != nil {
			t.Fatalf("request %d should stop on terminal 400 instead of falling back", i+1)
		}
		if result.lastErr == nil {
			t.Fatalf("request %d should return error", i+1)
		}
	}

	if badAttempts != 3 {
		t.Fatalf("badAttempts=%d, want=3 because terminal 400 should keep hitting the same channel", badAttempts)
	}
	if goodAttempts != 0 {
		t.Fatalf("goodAttempts=%d, want=0 because fallback should not be used for terminal 400", goodAttempts)
	}
}

func TestTryChannelsStatus400BalanceErrorOpensCircuit(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "balance-channel",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"retryable business failure"}}`)),
				}, nil
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
		Retry: RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code == http.StatusBadRequest &&
					strings.Contains(strings.ToLower(string(body)), retryableBusinessMarker)
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ctx != nil &&
				ctx.LastStatusCode == http.StatusBadRequest &&
				strings.Contains(strings.ToLower(string(ctx.LastResponseBody)), retryableBusinessMarker)
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 because 400 balance error should open circuit", badAttempts)
	}
	if goodAttempts != 3 {
		t.Fatalf("goodAttempts=%d, want=3 because fallback should be used on every request until breaker opens", goodAttempts)
	}
}

func TestTryChannelsStatus422OpensCircuit(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "unprocessable-channel",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusUnprocessableEntity,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
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
		Retry: NoRetry(),
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
		if i < 2 {
			if result.successResp != nil {
				t.Fatalf("request %d should stop on terminal 422 before breaker opens", i+1)
			}
			if result.lastErr == nil {
				t.Fatalf("request %d should return error", i+1)
			}
			continue
		}
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel after breaker opens", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 because 422 should open circuit after threshold", badAttempts)
	}
	if goodAttempts != 1 {
		t.Fatalf("goodAttempts=%d, want=1 because fallback should be used only after breaker opens", goodAttempts)
	}
}

func TestShouldCountChannelFailureForCircuitNetworkError(t *testing.T) {
	ctx := &Context{}
	ch := &Channel{Name: "network"}
	if !shouldCountChannelFailureForCircuit(ctx, ch, errors.New("dial tcp: i/o timeout")) {
		t.Fatalf("network retryable error should count for circuit")
	}
}

func TestTryChannelsCustomCircuitFailureDecider(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "bad-request-channel",
			BaseURL: "https://bad.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "bad-key", Value: "bad-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid request"}}`)),
				}, nil
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
		},
		ShouldCountFailureForCircuit: func(ctx *Context, ch *Channel, err error) bool {
			return ctx != nil && ch != nil && ch.Name == "bad-request-channel" && ctx.LastStatusCode == http.StatusBadRequest
		},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}

		result := p.tryChannels(req, ctx, channels, NoRetry())
		if i < 2 {
			if result.successResp != nil {
				t.Fatalf("request %d should stop on terminal 400 before custom breaker opens", i+1)
			}
			if result.lastErr == nil {
				t.Fatalf("request %d should return error", i+1)
			}
			continue
		}
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel after custom breaker opens", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if badAttempts != 2 {
		t.Fatalf("badAttempts=%d, want=2 because custom decider should open circuit", badAttempts)
	}
	if goodAttempts != 1 {
		t.Fatalf("goodAttempts=%d, want=1 because fallback should be used only after custom breaker opens", goodAttempts)
	}
}

func TestChannelCircuitBreakerCooldownBackoffUntilRecovery(t *testing.T) {
	now := time.Unix(1700000000, 0)
	breaker := newChannelCircuitBreaker("message", CircuitBreakerConfig{
		Enabled:            true,
		MinSamples:         1,
		ErrorRateThreshold: 1,
		FailureWindow:      time.Minute,
		Cooldown:           10 * time.Second,
	})
	breaker.now = func() time.Time { return now }

	ch := &Channel{Id: 1, Name: "bad"}

	opened, reopen, wait, reason := breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)
	if !opened || reopen {
		t.Fatalf("first open mismatch: opened=%v reopen=%v", opened, reopen)
	}
	if wait != 10*time.Second {
		t.Fatalf("first wait=%s, want=10s", wait)
	}
	if reason != "error_rate" {
		t.Fatalf("first reason=%s, want=error_rate", reason)
	}

	now = now.Add(11 * time.Second)
	if allowed, _, _ := breaker.Allow(ch); !allowed {
		t.Fatalf("channel should enter half-open after first cooldown")
	}

	opened, reopen, wait, _ = breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)
	if !opened || !reopen {
		t.Fatalf("second open mismatch: opened=%v reopen=%v", opened, reopen)
	}
	if wait != 20*time.Second {
		t.Fatalf("second wait=%s, want=20s", wait)
	}

	now = now.Add(21 * time.Second)
	if allowed, _, _ := breaker.Allow(ch); !allowed {
		t.Fatalf("channel should enter half-open after second cooldown")
	}

	if opened, wait, _ := breaker.RecordSuccess(ch, 0, false); opened || wait != 0 {
		t.Fatalf("half-open success should recover: opened=%v wait=%s", opened, wait)
	}

	opened, reopen, wait, _ = breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)
	if !opened || reopen {
		t.Fatalf("post-recovery open mismatch: opened=%v reopen=%v", opened, reopen)
	}
	if wait != 10*time.Second {
		t.Fatalf("post-recovery wait=%s, want reset to 10s", wait)
	}
}

// TestProxyCloseUnregistersBreaker 验证 Close 后 Proxy 的熔断器不再出现在全局快照中。
func TestProxyCloseUnregistersBreaker(t *testing.T) {
	newProxy := func(scope string) *Proxy {
		return New(Config{
			BreakerScope: scope,
			CircuitBreaker: CircuitBreakerConfig{
				Enabled:    true,
				MinSamples: 1,
			},
		})
	}
	hasScope := func(scope string) bool {
		for _, snapshot := range ListChannelHealthSnapshots() {
			if snapshot.Scope == scope {
				return true
			}
		}
		return false
	}

	p1 := newProxy("close-test-1")
	p2 := newProxy("close-test-2")
	defer p2.Close()
	ch := &Channel{Id: 1, Name: "ch"}
	p1.breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)
	p2.breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)

	if !hasScope("close-test-1") || !hasScope("close-test-2") {
		t.Fatal("both scopes should appear before Close")
	}

	p1.Close()
	if hasScope("close-test-1") {
		t.Fatal("closed proxy's breaker should be unregistered")
	}
	if !hasScope("close-test-2") {
		t.Fatal("other proxy's breaker should remain registered")
	}
}

// TestChannelCircuitBreakerCapsWindowSamples 验证窗口事件条数被 MaxWindowSamples 封顶。
func TestChannelCircuitBreakerCapsWindowSamples(t *testing.T) {
	now := time.Unix(1700000000, 0)
	breaker := newChannelCircuitBreaker("window-cap", CircuitBreakerConfig{
		Enabled:            true,
		MinSamples:         1000, // 不触发熔断，只看事件累计
		ErrorRateThreshold: 1,
		FailureWindow:      time.Hour,
		Cooldown:           time.Second,
		MaxWindowSamples:   5,
	})
	breaker.now = func() time.Time { return now }

	ch := &Channel{Id: 1, Name: "busy"}
	for i := 0; i < 10; i++ {
		breaker.RecordSuccess(ch, time.Millisecond, false)
		now = now.Add(time.Millisecond)
	}

	snapshots := breaker.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot len=%d, want=1", len(snapshots))
	}
	if got := snapshots[0].RequestCount; got != 5 {
		t.Fatalf("RequestCount=%d, want=5 (capped)", got)
	}
}

// errorAfterReader 先返回 data，读完后返回 err，模拟上游响应体半路断流。
type errorAfterReader struct {
	data []byte
	err  error
	pos  int
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

// TestRecordChannelSuccessTreatsBrokenSSEStreamAsFailure 验证 SSE 流半路断掉的渠道
// 在熔断器里按失败记账，而客户端取消导致的断流不惩罚渠道。
func TestRecordChannelSuccessTreatsBrokenSSEStreamAsFailure(t *testing.T) {
	runOnce := func(t *testing.T, streamErr error) *Proxy {
		t.Helper()
		channels := []Channel{
			{
				Id:      1,
				Name:    "sse",
				BaseURL: "https://sse.example.com",
				Enabled: true,
				GetKeys: func(ctx *Context) []Key {
					return []Key{{ID: "k", Value: "v"}}
				},
				Handler: func(ctx *Context) (*http.Response, error) {
					header := make(http.Header)
					header.Set("Content-Type", "text/event-stream")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     header,
						Body: io.NopCloser(&errorAfterReader{
							data: []byte("event: message_start\ndata: {}\n\n"),
							err:  streamErr,
						}),
					}, nil
				},
			},
		}

		p := New(Config{
			Retry: NoRetry(),
			CircuitBreaker: CircuitBreakerConfig{
				Enabled:            true,
				MinSamples:         1,
				ErrorRateThreshold: 1,
				FailureWindow:      time.Minute,
				Cooldown:           time.Minute,
			},
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request should succeed at the HTTP level")
		}
		p.writePipelineResponse(httptest.NewRecorder(), req, ctx, result)
		return p
	}

	t.Run("upstream read error opens circuit", func(t *testing.T) {
		p := runOnce(t, errors.New("unexpected EOF"))
		if allowed, _, _ := p.breaker.Allow(&Channel{Id: 1, Name: "sse"}); allowed {
			t.Fatalf("broken upstream stream should count as failure and open the circuit")
		}
		// 断流失败发生在 HTTP 200 之后：快照不应把 200 记成最后一次失败状态码
		snapshots := p.breaker.Snapshot()
		if len(snapshots) != 1 {
			t.Fatalf("snapshot len=%d, want=1", len(snapshots))
		}
		if got := snapshots[0].LastFailureStatusCode; got != 0 {
			t.Fatalf("LastFailureStatusCode=%d, want=0 for stream interruption", got)
		}
	})

	t.Run("client cancel does not punish channel", func(t *testing.T) {
		p := runOnce(t, context.Canceled)
		if allowed, _, _ := p.breaker.Allow(&Channel{Id: 1, Name: "sse"}); !allowed {
			t.Fatalf("client-cancel stream interruption should not open the circuit")
		}
	})
}

// TestServeHTTPReportsStreamInterruptionToOnRequestDone 验证上游断流时
// Observer.OnRequestDone 与熔断决策看到同一结论，而不是 doneErr=nil。
func TestServeHTTPReportsStreamInterruptionToOnRequestDone(t *testing.T) {
	obs := &captureDoneObserver{}
	p := New(Config{
		Observer: obs,
		Retry:    NoRetry(),
		Channels: []Channel{
			{
				Id:      1,
				Name:    "sse",
				BaseURL: "https://sse.example.com",
				Enabled: true,
				GetKeys: func(ctx *Context) []Key {
					return []Key{{ID: "k", Value: "v"}}
				},
				Handler: func(ctx *Context) (*http.Response, error) {
					header := make(http.Header)
					header.Set("Content-Type", "text/event-stream")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     header,
						Body: io.NopCloser(&errorAfterReader{
							data: []byte("event: message_start\ndata: {}\n\n"),
							err:  errors.New("unexpected EOF"),
						}),
					}, nil
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	p.ServeHTTP(httptest.NewRecorder(), req)

	if !obs.called {
		t.Fatal("OnRequestDone should be called")
	}
	if obs.doneErr == nil {
		t.Fatal("OnRequestDone should receive the stream interruption error")
	}
	if !strings.Contains(obs.doneErr.Error(), "upstream stream interrupted") {
		t.Fatalf("doneErr=%q, want stream interruption error", obs.doneErr)
	}
}

// TestNewDefaultsLowerWindowSamplesForRedisStore 验证 Redis store 下 MaxWindowSamples
// 默认取更小值（每次记账需全量序列化事件列表）。
func TestNewDefaultsLowerWindowSamplesForRedisStore(t *testing.T) {
	redisStore := NewRedisCircuitBreakerStore(nil, RedisCircuitBreakerStoreOptions{})
	p := New(Config{
		CircuitBreaker:      CircuitBreakerConfig{Enabled: true},
		CircuitBreakerStore: redisStore,
	})
	if got := p.breaker.cfg.MaxWindowSamples; got != 256 {
		t.Fatalf("redis store MaxWindowSamples=%d, want=256", got)
	}

	memProxy := New(Config{
		CircuitBreaker: CircuitBreakerConfig{Enabled: true},
	})
	if got := memProxy.breaker.cfg.MaxWindowSamples; got != 2048 {
		t.Fatalf("memory store MaxWindowSamples=%d, want=2048", got)
	}

	// 显式配置不被覆盖
	custom := New(Config{
		CircuitBreaker:      CircuitBreakerConfig{Enabled: true, MaxWindowSamples: 100},
		CircuitBreakerStore: redisStore,
	})
	if got := custom.breaker.cfg.MaxWindowSamples; got != 100 {
		t.Fatalf("explicit MaxWindowSamples=%d, want=100", got)
	}
}

// TestChannelCircuitBreakerCooldownCappedByMaxCooldown 验证连续熔断的指数冷却会被 MaxCooldown 封顶。
func TestChannelCircuitBreakerCooldownCappedByMaxCooldown(t *testing.T) {
	now := time.Unix(1700000000, 0)
	breaker := newChannelCircuitBreaker("cooldown-cap", CircuitBreakerConfig{
		Enabled:            true,
		MinSamples:         1,
		ErrorRateThreshold: 1,
		FailureWindow:      time.Minute,
		Cooldown:           10 * time.Second,
		MaxCooldown:        25 * time.Second,
	})
	breaker.now = func() time.Time { return now }

	ch := &Channel{Id: 1, Name: "bad"}

	wants := []time.Duration{
		10 * time.Second, // 第一次打开
		20 * time.Second, // 翻倍
		25 * time.Second, // 封顶
		25 * time.Second, // 保持封顶
	}
	for i, want := range wants {
		_, _, wait, _ := breaker.RecordFailure(ch, 0, false, http.StatusBadGateway)
		if wait != want {
			t.Fatalf("open %d: wait=%s, want=%s", i+1, wait, want)
		}
		// 等冷却结束，进入半开后再次失败触发下一轮
		now = now.Add(wait + time.Second)
		if allowed, _, probe := breaker.Allow(ch); !allowed || !probe {
			t.Fatalf("open %d: probe should be granted after cooldown", i+1)
		}
	}
}

// TestChannelCircuitBreakerAbandonedProbeReleasedAfterDeadline 验证半开探测被"放弃"
// （既没有记成功也没有记失败，例如钩子 panic）时，超过 ProbeTimeout 后
// 后续请求可以重新发起探测，渠道不会永久卡在半开状态。
func TestChannelCircuitBreakerAbandonedProbeReleasedAfterDeadline(t *testing.T) {
	now := time.Unix(1700000000, 0)
	breaker := newChannelCircuitBreaker("abandoned-probe", CircuitBreakerConfig{
		Enabled:            true,
		MinSamples:         1,
		ErrorRateThreshold: 1,
		FailureWindow:      time.Minute,
		Cooldown:           10 * time.Second,
		ProbeTimeout:       30 * time.Second,
	})
	breaker.now = func() time.Time { return now }

	ch := &Channel{Id: 1, Name: "bad"}

	if opened, _, _, _ := breaker.RecordFailure(ch, 0, false, http.StatusBadGateway); !opened {
		t.Fatalf("circuit should open after failure")
	}

	// 冷却结束后第一次探测被授予
	now = now.Add(11 * time.Second)
	if allowed, _, probe := breaker.Allow(ch); !allowed || !probe {
		t.Fatalf("first probe should be granted: allowed=%v probe=%v", allowed, probe)
	}

	// 探测在途（deadline 内）：其他请求被拒
	if allowed, _, _ := breaker.Allow(ch); allowed {
		t.Fatalf("requests during in-flight probe should be blocked")
	}

	// 探测被放弃：超过 ProbeTimeout 后应重新授予探测，而不是永久阻塞
	now = now.Add(31 * time.Second)
	if allowed, _, probe := breaker.Allow(ch); !allowed || !probe {
		t.Fatalf("abandoned probe should be released after deadline: allowed=%v probe=%v", allowed, probe)
	}

	// 新探测成功后渠道恢复
	if opened, _, _ := breaker.RecordSuccess(ch, 0, false); opened {
		t.Fatalf("probe success should recover the channel")
	}
	if allowed, _, probe := breaker.Allow(ch); !allowed || probe {
		t.Fatalf("recovered channel should allow normal traffic: allowed=%v probe=%v", allowed, probe)
	}
}

func TestChannelCircuitBreakerUsesSeparateThresholdForStreamAndNonStream(t *testing.T) {
	breaker := newChannelCircuitBreaker("message", CircuitBreakerConfig{
		Enabled:                true,
		MinSamples:             1,
		ErrorRateThreshold:     1,
		FailureWindow:          time.Minute,
		Cooldown:               time.Minute,
		StreamSlowThreshold:    5 * time.Second,
		NonStreamSlowThreshold: 12 * time.Second,
		SlowRateThreshold:      1,
	})

	streamCh := &Channel{Id: 1, Name: "stream"}
	if opened, _, reason := breaker.RecordSuccess(streamCh, 6*time.Second, true); !opened || reason != "slow_rate" {
		t.Fatalf("stream request should open by stream threshold: opened=%v reason=%s", opened, reason)
	}

	normalCh := &Channel{Id: 2, Name: "non-stream"}
	if opened, _, reason := breaker.RecordSuccess(normalCh, 6*time.Second, false); opened || reason != "" {
		t.Fatalf("non-stream request should not open by stream threshold: opened=%v reason=%s", opened, reason)
	}
}

func TestShouldCountNonPoolFailureForCircuit(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "bad_request", status: http.StatusBadRequest, want: false},
		{name: "forbidden", status: http.StatusForbidden, want: true},
		{name: "unauthorized", status: http.StatusUnauthorized, want: false},
		{name: "unprocessable_entity", status: http.StatusUnprocessableEntity, want: true},
		{name: "too_many_requests", status: http.StatusTooManyRequests, want: true},
		{name: "bad_gateway", status: http.StatusBadGateway, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &Context{LastStatusCode: tt.status}
			got := shouldCountNonPoolFailureForCircuit(ctx, errors.New("upstream error"))
			if got != tt.want {
				t.Fatalf("status=%d got=%v want=%v", tt.status, got, tt.want)
			}
		})
	}
}

func TestTryChannelsOpensCircuitAfterSlowRequests(t *testing.T) {
	slowAttempts := 0
	goodAttempts := 0
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
				slowAttempts++
				time.Sleep(20 * time.Millisecond)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
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
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         2,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Second,
			Cooldown:           time.Minute,
			SlowThreshold:      10 * time.Millisecond,
			SlowRateThreshold:  1,
		},
		BreakerScope: "message",
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed on fallback channel", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	if slowAttempts != 2 {
		t.Fatalf("slowAttempts=%d, want=2 after slow circuit opens", slowAttempts)
	}
	if goodAttempts != 1 {
		t.Fatalf("goodAttempts=%d, want=1 because fallback should be used only after circuit opens", goodAttempts)
	}

	snapshots := p.breaker.Snapshot()
	var slowSnapshot *ChannelHealthSnapshot
	for i := range snapshots {
		if snapshots[i].ChannelName == "slow" {
			slowSnapshot = &snapshots[i]
			break
		}
	}
	if slowSnapshot == nil {
		t.Fatalf("slow channel snapshot not found")
	}
	if slowSnapshot.OpenReason != "slow_rate" {
		t.Fatalf("openReason=%s, want=slow_rate", slowSnapshot.OpenReason)
	}
	if slowSnapshot.SlowCount != 2 {
		t.Fatalf("slowCount=%d, want=2", slowSnapshot.SlowCount)
	}
}

func TestTryChannelsSuccessfulRequestsAppearInWindowStats(t *testing.T) {
	successAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "healthy",
			BaseURL: "https://healthy.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "healthy-key", Value: "healthy-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				successAttempts++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
			},
		},
	}

	p := New(Config{
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         3,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Minute,
			Cooldown:           time.Minute,
		},
		BreakerScope: "message",
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
		ctx := &Context{Request: req}
		result := p.tryChannels(req, ctx, channels, NoRetry())
		if result.successResp == nil {
			t.Fatalf("request %d should succeed", i+1)
		}
		writeSuccessfulPipelineResult(t, p, req, ctx, result)
	}

	snapshots := p.breaker.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot len=%d, want=1", len(snapshots))
	}
	if snapshots[0].RequestCount != 3 {
		t.Fatalf("requestCount=%d, want=3", snapshots[0].RequestCount)
	}
	if snapshots[0].FailureCount != 0 {
		t.Fatalf("failureCount=%d, want=0", snapshots[0].FailureCount)
	}
	if successAttempts != 3 {
		t.Fatalf("successAttempts=%d, want=3", successAttempts)
	}
}

func TestServeHTTPSlowSSEUsesFirstEventTimeForCircuit(t *testing.T) {
	p := New(Config{
		Channels: []Channel{
			{
				Id:      1,
				Name:    "slow-stream",
				BaseURL: "https://slow-stream.example.com",
				Enabled: true,
				GetKeys: func(ctx *Context) []Key {
					return []Key{{ID: "slow-stream-key", Value: "slow-stream-value"}}
				},
				Handler: func(ctx *Context) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: &delayedReadCloser{
							payload: []byte("event: message\ndata: hello\n\n"),
							delay:   25 * time.Millisecond,
						},
					}, nil
				},
			},
		},
		Retry: NoRetry(),
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:                true,
			MinSamples:             1,
			ErrorRateThreshold:     1,
			FailureWindow:          time.Minute,
			Cooldown:               time.Minute,
			StreamSlowThreshold:    10 * time.Millisecond,
			NonStreamSlowThreshold: time.Second,
			SlowRateThreshold:      1,
		},
		BreakerScope: "message",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	w := httptest.NewRecorder()

	p.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusOK)
	}
	if body := w.Body.String(); !strings.Contains(body, "event: message") {
		t.Fatalf("body=%s", body)
	}

	snapshots := p.breaker.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot len=%d, want=1", len(snapshots))
	}
	if snapshots[0].OpenReason != "slow_rate" {
		t.Fatalf("openReason=%s, want=slow_rate", snapshots[0].OpenReason)
	}
	if snapshots[0].SlowCount != 1 {
		t.Fatalf("slowCount=%d, want=1", snapshots[0].SlowCount)
	}
	if snapshots[0].LastLatencyMode != "stream" {
		t.Fatalf("lastLatencyMode=%s, want=stream", snapshots[0].LastLatencyMode)
	}
}

type delayedReadCloser struct {
	payload []byte
	delay   time.Duration
	delayed bool
}

func (r *delayedReadCloser) Read(p []byte) (int, error) {
	if !r.delayed {
		time.Sleep(r.delay)
		r.delayed = true
	}
	if len(r.payload) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, nil
}

func (r *delayedReadCloser) Close() error {
	return nil
}

func TestChannelCircuitBreakerSnapshotIncludesLastFailureStatusCode(t *testing.T) {
	breaker := newChannelCircuitBreaker("message", CircuitBreakerConfig{
		Enabled:            true,
		MinSamples:         1,
		ErrorRateThreshold: 1,
		FailureWindow:      time.Minute,
		Cooldown:           time.Minute,
	})

	ch := &Channel{Id: 1, Name: "bad"}
	opened, _, _, _ := breaker.RecordFailure(ch, 0, false, http.StatusForbidden)
	if !opened {
		t.Fatalf("failure should open circuit")
	}

	snapshots := breaker.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot len=%d, want=1", len(snapshots))
	}
	if snapshots[0].LastFailureStatusCode != http.StatusForbidden {
		t.Fatalf("lastFailureStatusCode=%d, want=%d", snapshots[0].LastFailureStatusCode, http.StatusForbidden)
	}
}

func TestChannelCircuitBreakerSnapshotIncludesDetailedBreakdown(t *testing.T) {
	now := time.Unix(1700000000, 0)
	breaker := newChannelCircuitBreaker("message", CircuitBreakerConfig{
		Enabled:                true,
		MinSamples:             10,
		ErrorRateThreshold:     1,
		FailureWindow:          time.Minute,
		Cooldown:               15 * time.Second,
		StreamSlowThreshold:    10 * time.Millisecond,
		NonStreamSlowThreshold: 20 * time.Millisecond,
		SlowRateThreshold:      1,
	})
	breaker.now = func() time.Time { return now }

	ch := &Channel{Id: 9, Name: "mixed"}

	if opened, wait, _ := breaker.RecordSuccess(ch, 5*time.Millisecond, false); opened || wait != 0 {
		t.Fatalf("first success should not open: opened=%v wait=%s", opened, wait)
	}

	now = now.Add(time.Second)
	if opened, reopen, wait, _ := breaker.RecordFailure(ch, 25*time.Millisecond, false, http.StatusBadGateway); opened || reopen || wait != 0 {
		t.Fatalf("non-stream failure should not open: opened=%v reopen=%v wait=%s", opened, reopen, wait)
	}

	lastSuccessAt := now.Add(time.Second)
	now = lastSuccessAt
	if opened, wait, _ := breaker.RecordSuccess(ch, 15*time.Millisecond, true); opened || wait != 0 {
		t.Fatalf("stream success should not open: opened=%v wait=%s", opened, wait)
	}

	lastFailureAt := now.Add(time.Second)
	now = lastFailureAt
	if opened, reopen, wait, _ := breaker.RecordFailure(ch, 8*time.Millisecond, true, http.StatusTooManyRequests); opened || reopen || wait != 0 {
		t.Fatalf("stream failure should not open: opened=%v reopen=%v wait=%s", opened, reopen, wait)
	}

	snapshots := breaker.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot len=%d, want=1", len(snapshots))
	}

	snapshot := snapshots[0]
	if snapshot.RequestCount != 4 {
		t.Fatalf("requestCount=%d, want=4", snapshot.RequestCount)
	}
	if snapshot.SuccessCount != 2 {
		t.Fatalf("successCount=%d, want=2", snapshot.SuccessCount)
	}
	if snapshot.FailureCount != 2 {
		t.Fatalf("failureCount=%d, want=2", snapshot.FailureCount)
	}
	if snapshot.SlowCount != 2 {
		t.Fatalf("slowCount=%d, want=2", snapshot.SlowCount)
	}
	if snapshot.StreamRequestCount != 2 || snapshot.StreamSuccessCount != 1 || snapshot.StreamFailureCount != 1 || snapshot.StreamSlowCount != 1 {
		t.Fatalf("unexpected stream breakdown: %+v", snapshot)
	}
	if snapshot.NonStreamRequestCount != 2 || snapshot.NonStreamSuccessCount != 1 || snapshot.NonStreamFailureCount != 1 || snapshot.NonStreamSlowCount != 1 {
		t.Fatalf("unexpected non-stream breakdown: %+v", snapshot)
	}
	if snapshot.AvgLatencyMs != 13 {
		t.Fatalf("avgLatencyMs=%d, want=13", snapshot.AvgLatencyMs)
	}
	if snapshot.StreamAvgLatencyMs != 11 {
		t.Fatalf("streamAvgLatencyMs=%d, want=11", snapshot.StreamAvgLatencyMs)
	}
	if snapshot.NonStreamAvgLatencyMs != 15 {
		t.Fatalf("nonStreamAvgLatencyMs=%d, want=15", snapshot.NonStreamAvgLatencyMs)
	}
	if snapshot.MaxLatencyMs != 25 || snapshot.P95LatencyMs != 25 {
		t.Fatalf("unexpected overall latency stats: max=%d p95=%d", snapshot.MaxLatencyMs, snapshot.P95LatencyMs)
	}
	if snapshot.StreamMaxLatencyMs != 15 || snapshot.StreamP95LatencyMs != 15 {
		t.Fatalf("unexpected stream latency stats: max=%d p95=%d", snapshot.StreamMaxLatencyMs, snapshot.StreamP95LatencyMs)
	}
	if snapshot.NonStreamMaxLatencyMs != 25 || snapshot.NonStreamP95LatencyMs != 25 {
		t.Fatalf("unexpected non-stream latency stats: max=%d p95=%d", snapshot.NonStreamMaxLatencyMs, snapshot.NonStreamP95LatencyMs)
	}
	if snapshot.LastSuccessLatencyMs != 15 {
		t.Fatalf("lastSuccessLatencyMs=%d, want=15", snapshot.LastSuccessLatencyMs)
	}
	if snapshot.LastFailureLatencyMs != 8 {
		t.Fatalf("lastFailureLatencyMs=%d, want=8", snapshot.LastFailureLatencyMs)
	}
	if !snapshot.LastSuccessAt.Equal(lastSuccessAt) {
		t.Fatalf("lastSuccessAt=%s, want=%s", snapshot.LastSuccessAt, lastSuccessAt)
	}
	if !snapshot.LastFailureAt.Equal(lastFailureAt) {
		t.Fatalf("lastFailureAt=%s, want=%s", snapshot.LastFailureAt, lastFailureAt)
	}
	if snapshot.OpenRemainingSec != 0 {
		t.Fatalf("openRemainingSec=%d, want=0", snapshot.OpenRemainingSec)
	}
	if snapshot.ConsecutiveOpenCount != 0 {
		t.Fatalf("consecutiveOpenCount=%d, want=0", snapshot.ConsecutiveOpenCount)
	}
}

func TestProxyResetChannelHealthStats(t *testing.T) {
	p := New(Config{
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Minute,
			Cooldown:           time.Minute,
		},
	})

	bad := &Channel{Id: 1, Name: "bad", BaseURL: "https://bad.example.com"}
	good := &Channel{Id: 2, Name: "good", BaseURL: "https://good.example.com"}

	if opened, _, _, _ := p.breaker.RecordFailure(bad, 0, false, http.StatusBadGateway); !opened {
		t.Fatalf("bad failure should open circuit")
	}
	if opened, _, _, _ := p.breaker.RecordFailure(good, 0, false, http.StatusTooManyRequests); !opened {
		t.Fatalf("good failure should open circuit")
	}

	if ok := p.ResetChannelHealthStatsForChannel(bad); !ok {
		t.Fatalf("reset bad channel should succeed")
	}

	snapshots := p.breaker.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("snapshot len=%d, want=1 after per-channel reset", len(snapshots))
	}
	if snapshots[0].ChannelName != "good" {
		t.Fatalf("remaining snapshot channel=%s, want=good", snapshots[0].ChannelName)
	}

	if allowed, _, _ := p.breaker.Allow(bad); !allowed {
		t.Fatalf("bad channel should be closed after reset")
	}
	if allowed, _, _ := p.breaker.Allow(good); allowed {
		t.Fatalf("good channel should remain open before overall reset")
	}

	if resetCount := p.ResetChannelHealthStats(); resetCount != 1 {
		t.Fatalf("resetCount=%d, want=1", resetCount)
	}
	if snapshots := p.breaker.Snapshot(); len(snapshots) != 0 {
		t.Fatalf("snapshot len=%d, want=0 after overall reset", len(snapshots))
	}
	if allowed, _, _ := p.breaker.Allow(good); !allowed {
		t.Fatalf("good channel should be closed after overall reset")
	}
}

func TestResetChannelHealthStatsByKeyAcrossBreakers(t *testing.T) {
	ResetChannelHealthStats()

	p1 := New(Config{
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Minute,
			Cooldown:           time.Minute,
		},
		BreakerScope: "messages",
	})
	p2 := New(Config{
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Minute,
			Cooldown:           time.Minute,
		},
		BreakerScope: "embeddings",
	})

	shared := &Channel{Id: 99, Name: "shared", BaseURL: "https://shared.example.com"}

	if opened, _, _, _ := p1.breaker.RecordFailure(shared, 0, false, http.StatusBadGateway); !opened {
		t.Fatalf("p1 failure should open circuit")
	}
	if opened, _, _, _ := p2.breaker.RecordFailure(shared, 0, false, http.StatusBadGateway); !opened {
		t.Fatalf("p2 failure should open circuit")
	}

	if resetCount := ResetChannelHealthStatsByKey("id:99"); resetCount != 2 {
		t.Fatalf("resetCount=%d, want=2", resetCount)
	}
	if allowed, _, _ := p1.breaker.Allow(shared); !allowed {
		t.Fatalf("p1 shared channel should be closed after global reset")
	}
	if allowed, _, _ := p2.breaker.Allow(shared); !allowed {
		t.Fatalf("p2 shared channel should be closed after global reset")
	}
}

func TestCircuitBreakerStoreSharesStateAcrossProxies(t *testing.T) {
	store := newMemoryChannelCircuitStore()
	cfg := Config{
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Minute,
			Cooldown:           time.Minute,
		},
		CircuitBreakerStore: store,
		BreakerScope:        "messages",
	}
	p1 := New(cfg)
	p2 := New(cfg)

	ch := &Channel{Id: 100, Name: "shared-store", BaseURL: "https://shared-store.example.com"}
	if opened, _, _, _ := p1.breaker.RecordFailure(ch, 0, false, http.StatusBadGateway); !opened {
		t.Fatalf("p1 failure should open circuit")
	}
	if allowed, _, _ := p2.breaker.Allow(ch); allowed {
		t.Fatalf("p2 should see circuit opened by p1")
	}

	p3 := New(Config{
		CircuitBreaker: CircuitBreakerConfig{
			Enabled:            true,
			MinSamples:         1,
			ErrorRateThreshold: 1,
			FailureWindow:      time.Minute,
			Cooldown:           time.Minute,
		},
		CircuitBreakerStore: store,
		BreakerScope:        "embeddings",
	})
	if allowed, _, _ := p3.breaker.Allow(ch); !allowed {
		t.Fatalf("different breaker scope should not share circuit state")
	}
}
