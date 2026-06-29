package failover

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTryChannelsFailsOverOnEmptySSE 复现线上故障：上游返回 200 + text/event-stream + 空 body。
// 即使没有配置 RetryOnSSE，空流也必须判失败并切换到下一个渠道，而不是作为成功的 200 空 SSE
// 提交给客户端。
func TestTryChannelsFailsOverOnEmptySSE(t *testing.T) {
	badAttempts := 0
	goodAttempts := 0
	channels := []Channel{
		{
			Id:      1,
			Name:    "empty-sse",
			BaseURL: "https://empty.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k1", Value: "v1"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				badAttempts++
				h := make(http.Header)
				h.Set("Content-Type", "text/event-stream")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("")), // 空 body
				}, nil
			},
		},
		{
			Id:      2,
			Name:    "good",
			BaseURL: "https://good.example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: "k2", Value: "v2"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				goodAttempts++
				h := make(http.Header)
				h.Set("Content-Type", "text/event-stream")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("event: message_start\ndata: {}\n\n")),
				}, nil
			},
		},
	}

	// 默认零值 RetryConfig（未配置 RetryOnSSE），模拟未显式接入 SSE 钩子的部署。
	p := New(Config{Retry: RetryConfig{MaxAttempts: 1}})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Accept", "text/event-stream")
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp == nil {
		t.Fatalf("empty SSE should fail over to good channel, lastErr=%v", result.lastErr)
	}

	rec := httptest.NewRecorder()
	p.writePipelineResponse(rec, req, ctx, result)

	if badAttempts != 1 {
		t.Fatalf("badAttempts=%d, want=1", badAttempts)
	}
	if goodAttempts != 1 {
		t.Fatalf("goodAttempts=%d, want=1 (must fail over off empty SSE)", goodAttempts)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("client status=%d, want=200 from good channel", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "message_start") {
		t.Fatalf("client body=%q, want good channel SSE", rec.Body.String())
	}
}

// TestTryChannelsEmptySSEAllChannelsFail 验证当所有渠道都返回空 SSE 时，最终不会给客户端
// 返回 200 + 空 SSE，而是明确的失败状态码（502）。
func TestTryChannelsEmptySSEAllChannelsFail(t *testing.T) {
	emptyHandler := func(ctx *Context) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Content-Type", "text/event-stream")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}
	channels := []Channel{
		{
			Id: 1, Name: "empty-1", BaseURL: "https://e1.example.com", Enabled: true,
			GetKeys: func(ctx *Context) []Key { return []Key{{ID: "k1", Value: "v1"}} },
			Handler: emptyHandler,
		},
		{
			Id: 2, Name: "empty-2", BaseURL: "https://e2.example.com", Enabled: true,
			GetKeys: func(ctx *Context) []Key { return []Key{{ID: "k2", Value: "v2"}} },
			Handler: emptyHandler,
		},
	}

	p := New(Config{Retry: RetryConfig{MaxAttempts: 1}})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Accept", "text/event-stream")
	ctx := &Context{Request: req}
	result := p.tryChannels(req, ctx, channels, p.cfg.Retry)
	if result.successResp != nil {
		t.Fatalf("all-empty-SSE must not be treated as success")
	}

	rec := httptest.NewRecorder()
	p.writePipelineResponse(rec, req, ctx, result)

	if rec.Code == http.StatusOK {
		t.Fatalf("client status=200, want a failure status (e.g. 502) when all channels return empty SSE")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("client status=%d, want=502", rec.Code)
	}
}
