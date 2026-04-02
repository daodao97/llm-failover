package failover

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrepareChannelAttemptRunsBeforeProxyBeforeGetKeys(t *testing.T) {
	p := &Proxy{
		cfg: Config{
			BeforeProxy: func(ctx *Context) func(*Context) (*http.Response, error) {
				return func(_ *Context) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					}, nil
				}
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := &Context{Request: req}

	getKeysCalled := false
	ch := &Channel{
		Name:    "short-circuit",
		BaseURL: "https://example.com",
		GetKeys: func(ctx *Context) []Key {
			getKeysCalled = true
			return nil
		},
	}

	_, _, internalHandler, err := p.prepareChannelAttempt(req, ctx, ch)
	if err != nil {
		t.Fatalf("prepareChannelAttempt error: %v", err)
	}
	if internalHandler == nil {
		t.Fatal("expected internal handler")
	}
	if getKeysCalled {
		t.Fatal("expected GetKeys not to be called before BeforeProxy short-circuit")
	}
}
