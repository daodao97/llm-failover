package failover

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShouldContinueToNextChannelDefaultsWithoutRetryOnResponse 验证 RetryOnResponse 未配置时，
// 429 和 5xx 默认继续切换下一个渠道，其他 4xx 不切换。
func TestShouldContinueToNextChannelDefaultsWithoutRetryOnResponse(t *testing.T) {
	p := New(Config{})
	ch := &Channel{Id: 1, Name: "test"}

	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusUnprocessableEntity, false},
	}
	for _, tc := range cases {
		ctx := &Context{LastStatusCode: tc.status}
		got := p.shouldContinueToNextChannel(ctx, ch, RetryConfig{}, errors.New("status: failed"))
		if got != tc.want {
			t.Fatalf("status=%d: shouldContinueToNextChannel=%v, want=%v", tc.status, got, tc.want)
		}
	}
}

// TestTryChannelsFailsOverOn5xxWithoutRetryOnResponse 复现 README 的 Cloudflare 示例配置
// （只设置 MaxAttempts 的零值 RetryConfig），主渠道 5xx 时仍应切换到备用渠道。
func TestTryChannelsFailsOverOn5xxWithoutRetryOnResponse(t *testing.T) {
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
					StatusCode: http.StatusInternalServerError,
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
		Retry: RetryConfig{MaxAttempts: 1},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp == nil {
		t.Fatalf("request should fail over to good channel on 500, lastErr=%v", result.lastErr)
	}
	p.writePipelineResponse(httptest.NewRecorder(), req, ctx, result)

	if badAttempts != 1 {
		t.Fatalf("badAttempts=%d, want=1", badAttempts)
	}
	if goodAttempts != 1 {
		t.Fatalf("goodAttempts=%d, want=1", goodAttempts)
	}
}

// TestTryChannelsDoesNotFailOverOn400WithoutRetryOnResponse 验证默认行为下 400 类请求错误
// 不应切换渠道（换渠道也无法成功，徒增上游压力）。
func TestTryChannelsDoesNotFailOverOn400WithoutRetryOnResponse(t *testing.T) {
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
					StatusCode: http.StatusBadRequest,
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
		Retry: RetryConfig{MaxAttempts: 1},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp != nil {
		t.Fatalf("400 should not fail over to next channel")
	}
	p.writePipelineResponse(httptest.NewRecorder(), req, ctx, result)

	if badAttempts != 1 {
		t.Fatalf("badAttempts=%d, want=1", badAttempts)
	}
	if goodAttempts != 0 {
		t.Fatalf("goodAttempts=%d, want=0 because 400 should not trigger failover", goodAttempts)
	}
}
