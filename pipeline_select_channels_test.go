package failover

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelectChannelsReturns503WhenNoChannelsAvailable(t *testing.T) {
	p := New(Config{
		GetChannels: func(r *http.Request) ([]Channel, error) {
			return nil, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	w := httptest.NewRecorder()
	ctx := &Context{Request: req}

	channels, ok := p.selectChannels(w, req, ctx)
	if ok {
		t.Fatal("expected selectChannels to fail")
	}
	if channels != nil {
		t.Fatalf("channels=%v, want nil", channels)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(w.Body.String(), "no channels available") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestClassifySelectChannelsStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "service not initialized",
			err:  errors.New("channelHelper not initialized"),
			want: http.StatusServiceUnavailable,
		},
		{
			name: "request scoped config mismatch",
			err:  errors.New("no channels configured for token"),
			want: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySelectChannelsStatus(tt.err); got != tt.want {
				t.Fatalf("status=%d, want=%d", got, tt.want)
			}
		})
	}
}

func TestServeHTTPGetChannelsCanReadRequestBody(t *testing.T) {
	selected := ""
	seenBody := ""

	buildChannel := func(id int, name string) Channel {
		return Channel{
			Id:      id,
			Name:    name,
			BaseURL: "https://" + name + ".example.com",
			Enabled: true,
			GetKeys: func(ctx *Context) []Key {
				return []Key{{ID: name + "-key", Value: name + "-value"}}
			},
			Handler: func(ctx *Context) (*http.Response, error) {
				selected = name
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"channel":"` + name + `"}`)),
				}, nil
			},
		}
	}

	p := New(Config{
		GetChannels: func(r *http.Request) ([]Channel, error) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			seenBody = string(data)
			if strings.Contains(seenBody, `"model":"route-b"`) {
				return []Channel{buildChannel(2, "route-b")}, nil
			}
			return []Channel{buildChannel(1, "route-a")}, nil
		},
		Retry: NoRetry(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"route-b"}`))
	w := httptest.NewRecorder()

	p.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want=%d", w.Code, http.StatusOK)
	}
	if selected != "route-b" {
		t.Fatalf("selected=%q, want=%q", selected, "route-b")
	}
	if !strings.Contains(seenBody, `"model":"route-b"`) {
		t.Fatalf("seenBody=%q", seenBody)
	}
	if body := w.Body.String(); !strings.Contains(body, `"channel":"route-b"`) {
		t.Fatalf("body=%s", body)
	}
}
