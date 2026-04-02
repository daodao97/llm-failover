package failover

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTryChannelDoesNotForwardSensitiveClientHeaders(t *testing.T) {
	var forwarded http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	p := New(Config{
		Client: server.Client(),
		Retry:  NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Cookie", "session=client-cookie")
	req.Header.Set("X-API-Key", "client-api-key")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Trace-Id", "trace-1")

	ctx := &Context{Request: req}
	if !p.prepareRequestBody(httptest.NewRecorder(), ctx) {
		t.Fatal("prepareRequestBody should succeed")
	}

	resp, err := p.tryChannel(req, ctx, &Channel{
		Id:      1,
		Name:    "third",
		BaseURL: server.URL,
		Enabled: true,
		KeyType: "api_key",
		GetKeys: func(ctx *Context) []Key {
			return []Key{{ID: "k1", Value: "upstream-api-key"}}
		},
	}, NoRetry())
	if err != nil {
		t.Fatalf("tryChannel error: %v", err)
	}
	resp.Body.Close()

	if forwarded.Get("Authorization") != "" {
		t.Fatalf("authorization leaked: %q", forwarded.Get("Authorization"))
	}
	if forwarded.Get("Cookie") != "" {
		t.Fatalf("cookie leaked: %q", forwarded.Get("Cookie"))
	}
	if got := forwarded.Get("X-Api-Key"); got != "upstream-api-key" {
		t.Fatalf("x-api-key=%q, want=%q", got, "upstream-api-key")
	}
	if got := forwarded.Get("X-Trace-Id"); got != "trace-1" {
		t.Fatalf("x-trace-id=%q, want=%q", got, "trace-1")
	}
}

func TestExecuteChannelHandlerAttemptReleasesKeyOnBodyClose(t *testing.T) {
	releaseCount := 0
	acquireCount := 0
	bodyClosed := 0

	p := &Proxy{}
	resp, err := p.executeChannelHandlerAttempt(&Context{}, &Channel{
		AcquireKey: func(keyID string) bool {
			acquireCount++
			return true
		},
		ReleaseKey: func(keyID string) {
			releaseCount++
		},
		Handler: func(ctx *Context) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: readCloserFunc{
					read: func(p []byte) (int, error) {
						return 0, io.EOF
					},
					close: func() error {
						bodyClosed++
						return nil
					},
				},
			}, nil
		},
	}, &Key{ID: "k1"})
	if err != nil {
		t.Fatalf("executeChannelHandlerAttempt error: %v", err)
	}
	if acquireCount != 1 {
		t.Fatalf("acquireCount=%d, want=1", acquireCount)
	}
	if releaseCount != 0 {
		t.Fatalf("releaseCount=%d, want=0 before body close", releaseCount)
	}

	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}
	if releaseCount != 1 {
		t.Fatalf("releaseCount=%d, want=1 after body close", releaseCount)
	}
	if bodyClosed != 1 {
		t.Fatalf("bodyClosed=%d, want=1", bodyClosed)
	}
}

type readCloserFunc struct {
	read  func(p []byte) (int, error)
	close func() error
}

func (r readCloserFunc) Read(p []byte) (int, error) {
	return r.read(p)
}

func (r readCloserFunc) Close() error {
	return r.close()
}
