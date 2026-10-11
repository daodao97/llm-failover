package failover

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetChannelsRequestErrorControlsTypeAndStatus(t *testing.T) {
	p := New(Config{
		GetChannels: func(*http.Request) ([]Channel, error) {
			return nil, NewRequestError("invalid_request_error", http.StatusBadRequest, errors.New("model cannot be routed"))
		},
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request_error"`) || !strings.Contains(rec.Body.String(), "model cannot be routed") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestWriteSSEEvent(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSSEEvent(&buf, SSEEvent{Event: "message", ID: "1", Data: "a\nb"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "event: message\nid: 1\ndata: a\ndata: b\n\n" {
		t.Fatalf("got %q", got)
	}
}
