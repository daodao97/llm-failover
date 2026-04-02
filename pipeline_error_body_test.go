package failover

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestServeHTTPPrefersUpstreamErrorBodyAfterRetryExhausted(t *testing.T) {
	attempts := 0
	ch := Channel{
		Id:      101,
		Name:    "test-pool",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"upstream bad request"}}`)),
			}, nil
		},
		Retry: &RetryConfig{
			MaxAttempts: 2,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code >= http.StatusBadRequest
			},
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry:    NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if attempts != 2 {
		t.Fatalf("attempts=%d, want=2", attempts)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusBadRequest)
	}

	body := w.Body.String()
	if strings.Contains(body, "all channels failed") {
		t.Fatalf("should return upstream error body, got=%s", body)
	}
	if !strings.Contains(body, "upstream bad request") {
		t.Fatalf("missing upstream message, got=%s", body)
	}
	if got := gjson.Get(body, "error.type").String(); got != "proxy_error" {
		t.Fatalf("error.type=%q, want=%q", got, "proxy_error")
	}
}

func TestServeHTTPFallsBackToStatusWhenNoErrorBody(t *testing.T) {
	attempts := 0
	ch := Channel{
		Id:      102,
		Name:    "test-pool-empty-body",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader("")),
			}, nil
		},
		Retry: &RetryConfig{
			MaxAttempts: 2,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code >= http.StatusBadRequest
			},
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry:    NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if attempts != 2 {
		t.Fatalf("attempts=%d, want=2", attempts)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusBadRequest)
	}

	msg := gjson.Get(w.Body.String(), "error.message").String()
	if !strings.Contains(msg, "all channels failed: ["+MaskChannelName("102")+"] retry: status: 400") {
		t.Fatalf("error.message=%q", msg)
	}
}

func TestServeHTTPWrapsNonJSONErrorBodyAsJSON(t *testing.T) {
	ch := Channel{
		Id:      104,
		Name:    "test-pool-html-error",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header: http.Header{
					"Content-Type": []string{"text/html; charset=utf-8"},
				},
				Body: io.NopCloser(strings.NewReader(`<!DOCTYPE html><html><body>bad gateway</body></html>`)),
			}, nil
		},
		Retry: &RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code >= http.StatusBadRequest
			},
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry:    NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusBadGateway)
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type=%q", got)
	}

	body := w.Body.String()
	if strings.Contains(strings.ToLower(body), "<html") {
		t.Fatalf("should not leak html body, got=%s", body)
	}
	msg := gjson.Get(body, "error.message").String()
	if !strings.Contains(msg, "["+MaskChannelName("104")+"] upstream service returned non-json error response: Bad Gateway") {
		t.Fatalf("error.message=%q", msg)
	}
	if got := gjson.Get(body, "error.type").String(); got != "proxy_error" {
		t.Fatalf("error.type=%q, want=%q", got, "proxy_error")
	}
}

func TestServeHTTPIncludesChannelMaskInRetryMessage(t *testing.T) {
	ch := Channel{
		Id:      103,
		Name:    "test-pool-internal-error",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"internal server error"}}`)),
			}, nil
		},
		Retry: &RetryConfig{
			MaxAttempts: 1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool {
				return code >= http.StatusInternalServerError
			},
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry:    NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	msg := gjson.Get(w.Body.String(), "error.message").String()
	if !strings.Contains(msg, "["+MaskChannelName("103")+"] internal server error") {
		t.Fatalf("error.message=%q", msg)
	}
}

func TestServeHTTPSanitizesRequestIDAnnotationsInJSONErrorBody(t *testing.T) {
	ch := Channel{
		Id:      106,
		Name:    "test-pool-request-id",
		Enabled: true,
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "v1"}}
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(`{"error":{"type":"<nil>","message":"prompt is too long: 221543 tokens > 200000 maximum (request id: 20260401134441150368287E5NgdRae) (request id: 2026040113444125668612HZOXAcsO)"}}`)),
			}, nil
		},
		Retry: &RetryConfig{
			MaxAttempts: 1,
		},
	}

	p := New(Config{
		Channels: []Channel{ch},
		Retry:    NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusBadRequest)
	}

	msg := gjson.Get(w.Body.String(), "error.message").String()
	if strings.Contains(strings.ToLower(msg), "request id:") {
		t.Fatalf("error.message should strip request id annotations, got=%q", msg)
	}
	if !strings.Contains(msg, "prompt is too long: 221543 tokens > 200000 maximum") {
		t.Fatalf("error.message=%q", msg)
	}
}

func TestServeHTTPGeneratedErrorSanitizesUpstreamURL(t *testing.T) {
	p := New(Config{
		Channels: []Channel{
			{
				Id:      105,
				Name:    "test-timeout-channel",
				Enabled: true,
				GetKeys: func(ctx *Context) []Key {
					return []Key{{ID: "k1", Value: "v1"}}
				},
				Handler: func(ctx *Context) (*http.Response, error) {
					return nil, errors.New(`Post "http://10.100.31.32:4101/v1/messages?beta=true": EOF`)
				},
			},
		},
		Retry: NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusBadGateway)
	}

	msg := gjson.Get(w.Body.String(), "error.message").String()
	if strings.Contains(msg, "10.100.31.32") || strings.Contains(msg, "http://") || strings.Contains(msg, "beta=true") {
		t.Fatalf("error.message should hide upstream url, got=%q", msg)
	}
	if !strings.Contains(msg, `Post "[upstream-url]": EOF`) {
		t.Fatalf("error.message=%q", msg)
	}
}

func TestServeHTTPLocalSelectionErrorIncludesTracePrefix(t *testing.T) {
	p := New(Config{
		GetChannels: func(r *http.Request) ([]Channel, error) {
			return nil, io.ErrUnexpectedEOF
		},
		Retry: NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req = req.WithContext(SetTraceID(context.Background(), "trace-400"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusBadRequest)
	}

	msg := gjson.Get(w.Body.String(), "error.message").String()
	if !strings.Contains(msg, "[trace-400]") {
		t.Fatalf("error.message=%q", msg)
	}
	if !strings.Contains(msg, io.ErrUnexpectedEOF.Error()) {
		t.Fatalf("error.message=%q", msg)
	}
}
