package failover

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClientFailureMessage(t *testing.T) {
	ch := &Channel{Id: 78, ChannelGroupID: 10}
	label := ChannelLabel(ch)
	cases := []struct {
		name      string
		err       error
		involved  int
		wantLabel string
		wantMsg   string
	}{
		{"single attempt", wrapChannelError(ch, fmt.Errorf("retry: %s", "sse_error: empty response")), 1, label, "upstream request failed: sse_error: empty response"},
		{"circuit open", wrapChannelError(ch, errCircuitOpen), 1, label, "channel temporarily unavailable: circuit open"},
		{"several channels", wrapChannelError(ch, errors.New("status: 503")), 3, label, "all channels failed: status: 503"},
		{"unlabeled", errors.New("failover timeout exceeded"), 2, "", "all channels failed: failover timeout exceeded"},
	}
	for _, tc := range cases {
		gotLabel, gotMsg := clientFailureMessage(tc.err, tc.involved)
		if gotLabel != tc.wantLabel || gotMsg != tc.wantMsg {
			t.Fatalf("%s: got (%q, %q), want (%q, %q)", tc.name, gotLabel, gotMsg, tc.wantLabel, tc.wantMsg)
		}
	}
}

func TestWrapChannelErrorKeepsLogFormat(t *testing.T) {
	ch := &Channel{Id: 78, ChannelGroupID: 10}
	err := wrapChannelError(ch, wrapChannelError(ch, ErrAttemptTimeout))
	if want := "[" + MaskChannelName("78") + " 10] attempt timeout exceeded"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, ErrAttemptTimeout) {
		t.Fatal("wrapped error should unwrap to cause")
	}
}

func TestServeHTTPFailurePrefixShowsTraceAndGroupOnce(t *testing.T) {
	ch := Channel{
		Id:             78,
		Name:           "test-group-channel",
		ChannelGroupID: 10,
		Enabled:        true,
		GetKeys:        func(ctx *Context) []Key { return []Key{{ID: "k1", Value: "k1"}} },
		Handler: func(ctx *Context) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		},
		Retry: &RetryConfig{
			MaxAttempts:     1,
			RetryOnResponse: func(ctx *Context, ch *Channel, code int, body []byte) bool { return true },
		},
	}
	p := New(Config{Channels: []Channel{ch}, Retry: NoRetry()})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req = req.WithContext(SetTraceID(req.Context(), "trace-1"))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	msg := gjson.Get(w.Body.String(), "error.message").String()
	if want := "[trace-1 " + MaskChannelName("78") + " 10] upstream request failed: status: 502"; msg != want {
		t.Fatalf("error.message = %q, want %q", msg, want)
	}
}
